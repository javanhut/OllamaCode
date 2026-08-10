package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/javanhut/ollama_code/tools"
)

// maxSnapshotBytes bounds per-file checkpoint memory; larger files are not
// snapshotted (and so can't be undone), which is noted to the user on /undo.
const maxSnapshotBytes = 10 * 1024 * 1024

// fileSnap captures a file's state before a mutating tool touched it.
type fileSnap struct {
	existed bool
	tooBig  bool
	data    []byte
	mode    os.FileMode
}

// turnCheckpoint groups all file snapshots taken during one user turn.
type turnCheckpoint struct {
	label string
	snaps map[string]fileSnap
}

// checkpointStore holds per-turn snapshots and the undo stack. All access is
// mutex-guarded because snapshots are taken from tool goroutines while /undo and
// finalize run on the UI loop.
type checkpointStore struct {
	mu      sync.Mutex
	pending map[string]fileSnap // current turn, keyed by path
	stack   []turnCheckpoint
}

const maxUndoDepth = 25

// checkpointBeforeCall returns the executor Before hook that snapshots every
// file a mutating tool call is about to touch. Direct tool calls and sub-agent
// runs share it, so delegated writes land in the parent turn's checkpoint and
// one /undo rewinds the whole delegation. First-version-wins per path: a file
// already snapshotted this turn is never re-read.
func (m *Model) checkpointBeforeCall() func(tools.ToolCall) {
	return func(call tools.ToolCall) {
		if paths := tools.MutatedPaths(call.Function.Name, call.Function.Arguments); len(paths) > 0 {
			m.snapshotBeforeMutate(paths)
		}
	}
}

// snapshotBeforeMutate records the current state of each path before a mutating
// tool runs, once per path per turn. Safe to call from a tool goroutine.
func (m *Model) snapshotBeforeMutate(paths []string) {
	m.ckpt.mu.Lock()
	defer m.ckpt.mu.Unlock()
	if m.ckpt.pending == nil {
		m.ckpt.pending = map[string]fileSnap{}
	}
	for _, p := range paths {
		if _, seen := m.ckpt.pending[p]; seen {
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			m.ckpt.pending[p] = fileSnap{existed: false}
			continue
		}
		if info.Size() > maxSnapshotBytes {
			m.ckpt.pending[p] = fileSnap{existed: true, tooBig: true, mode: info.Mode().Perm()}
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			m.ckpt.pending[p] = fileSnap{existed: false}
			continue
		}
		m.ckpt.pending[p] = fileSnap{existed: true, data: data, mode: info.Mode().Perm()}
	}
}

// finalizeCheckpoint pushes the current turn's snapshots onto the undo stack.
// Called at turn end. The stack-only push is a no-op when nothing was mutated,
// but the turn-end auto-save runs either way.
func (m *Model) finalizeCheckpoint(label string) {
	m.ckpt.mu.Lock()
	if len(m.ckpt.pending) > 0 {
		m.ckpt.stack = append(m.ckpt.stack, turnCheckpoint{label: label, snaps: m.ckpt.pending})
		if len(m.ckpt.stack) > maxUndoDepth {
			m.ckpt.stack = m.ckpt.stack[len(m.ckpt.stack)-maxUndoDepth:]
		}
		m.ckpt.pending = nil
		m.persistCheckpointsLocked()
	}
	m.ckpt.mu.Unlock()
	m.autosaveSession()
}

// maxPersistedCheckpointBytes bounds the on-disk checkpoint file. Snapshots
// hold whole file contents, so without a budget a rewrite-heavy session could
// pile up hundreds of MB on disk; oldest checkpoints are dropped first, which
// matches how the in-memory stack prunes.
const maxPersistedCheckpointBytes = 32 << 20

// persistedSnap / persistedTurn are the on-disk forms of fileSnap and
// turnCheckpoint (the in-memory types have unexported fields).
type persistedSnap struct {
	Existed bool        `json:"existed"`
	TooBig  bool        `json:"too_big,omitempty"`
	Data    []byte      `json:"data,omitempty"`
	Mode    os.FileMode `json:"mode,omitempty"`
}

type persistedTurn struct {
	Label string                   `json:"label"`
	Snaps map[string]persistedSnap `json:"snaps"`
}

// persistCheckpointsLocked writes the undo stack to its per-workspace file so
// /undo survives a restart. Newest checkpoints win the size budget; the write
// is atomic and best-effort. Callers must hold m.ckpt.mu.
func (m *Model) persistCheckpointsLocked() {
	if !sessionPersist.Load() {
		return
	}
	keep := len(m.ckpt.stack)
	total := 0
	for i := len(m.ckpt.stack) - 1; i >= 0; i-- {
		size := 0
		for _, s := range m.ckpt.stack[i].snaps {
			size += len(s.data)
		}
		if total+size > maxPersistedCheckpointBytes && keep < len(m.ckpt.stack) {
			break
		}
		total += size
		keep = i
	}
	turns := make([]persistedTurn, 0, len(m.ckpt.stack)-keep)
	for _, cp := range m.ckpt.stack[keep:] {
		pt := persistedTurn{Label: cp.label, Snaps: make(map[string]persistedSnap, len(cp.snaps))}
		for path, s := range cp.snaps {
			pt.Snaps[path] = persistedSnap{Existed: s.existed, TooBig: s.tooBig, Data: s.data, Mode: s.mode}
		}
		turns = append(turns, pt)
	}
	data, err := json.Marshal(turns)
	if err != nil {
		return
	}
	_ = writeFileAtomic(checkpointPath(), data, 0o644)
}

// loadPersistedCheckpoints restores the undo stack written by a previous
// process. Called on resume; a missing or corrupt file just means an empty
// stack. In-memory data wins on shape mismatch: anything that fails to decode
// is dropped rather than partially trusted.
func (m *Model) loadPersistedCheckpoints() {
	data, err := os.ReadFile(checkpointPath())
	if err != nil {
		return
	}
	var turns []persistedTurn
	if err := json.Unmarshal(data, &turns); err != nil {
		return
	}
	if len(turns) > maxUndoDepth {
		turns = turns[len(turns)-maxUndoDepth:]
	}
	stack := make([]turnCheckpoint, 0, len(turns))
	for _, pt := range turns {
		cp := turnCheckpoint{label: pt.Label, snaps: make(map[string]fileSnap, len(pt.Snaps))}
		for path, s := range pt.Snaps {
			cp.snaps[path] = fileSnap{existed: s.Existed, tooBig: s.TooBig, data: s.Data, mode: s.Mode}
		}
		stack = append(stack, cp)
	}
	m.ckpt.mu.Lock()
	m.ckpt.stack = stack
	m.ckpt.mu.Unlock()
}

// undoLast restores the most recent turn's file changes. Returns a human summary
// and the paths it touched (so the caller can refresh the RAG index).
func (m *Model) undoLast() (string, []string) {
	m.ckpt.mu.Lock()
	defer m.ckpt.mu.Unlock()
	if len(m.ckpt.stack) == 0 {
		return "nothing to undo", nil
	}
	cp := m.ckpt.stack[len(m.ckpt.stack)-1]
	m.ckpt.stack = m.ckpt.stack[:len(m.ckpt.stack)-1]
	m.persistCheckpointsLocked() // keep the on-disk stack in sync with the pop

	restored, deleted, skipped := 0, 0, 0
	var touched []string
	for path, s := range cp.snaps {
		touched = append(touched, path)
		switch {
		case s.tooBig:
			skipped++
		case !s.existed:
			// File was created during the turn → remove it to undo.
			if err := os.Remove(path); err == nil {
				deleted++
			}
		default:
			mode := s.mode
			if mode == 0 {
				mode = 0o644
			}
			if err := os.WriteFile(path, s.data, mode); err == nil {
				restored++
			}
		}
	}
	msg := fmt.Sprintf("undid \"%s\": %d file(s) restored, %d removed", cp.label, restored, deleted)
	if skipped > 0 {
		msg += fmt.Sprintf(", %d skipped (too large to snapshot)", skipped)
	}
	return msg, touched
}

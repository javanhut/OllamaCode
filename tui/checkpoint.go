package tui

import (
	"fmt"
	"os"
	"sync"
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
// Called at turn end. No-op when nothing was mutated.
func (m *Model) finalizeCheckpoint(label string) {
	m.ckpt.mu.Lock()
	defer m.ckpt.mu.Unlock()
	if len(m.ckpt.pending) == 0 {
		return
	}
	m.ckpt.stack = append(m.ckpt.stack, turnCheckpoint{label: label, snaps: m.ckpt.pending})
	if len(m.ckpt.stack) > maxUndoDepth {
		m.ckpt.stack = m.ckpt.stack[len(m.ckpt.stack)-maxUndoDepth:]
	}
	m.ckpt.pending = nil
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

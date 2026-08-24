package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/javanhut/ollama_code/tools"
)

// Undo is git-backed. A shadow repository — its own GIT_DIR under the ocode
// state dir, the workspace as its work-tree — records the whole workspace as a
// tree object before the first mutating tool call of a turn, and /undo puts
// that tree back. The user's own repository is never touched: separate git dir,
// separate index, and we write no commits, refs or HEAD, so their staging area,
// branch, stash and reflog are exactly as they left them. It works in a
// directory that is not a repo at all — the shadow repo is ours.
//
// This replaces hand-rolled snapshots that held whole file contents in memory
// and re-serialized them to JSON every turn. Git compresses and dedups, so the
// 10MB per-file cap and the 32MB on-disk budget are gone (25 snapshots of a
// repo cost about one copy plus the churn), restore is exact — files the turn
// created are deleted, files it deleted come back, the mode bit is preserved —
// and surviving a restart costs a list of tree ids instead of a file dump.
//
// Three things it does not cover: paths .gitignore excludes, paths outside the
// workspace root reached through the jail allowlist, and the contents of a
// nested repository (git stages one as a gitlink, so a revert leaves its files
// alone — it can't lose them, but it can't restore them either). The first is
// the price of a snapshot that stays bounded — an unignored dependency tree
// would make every turn hash a gigabyte — and the other two were always
// corners.

const maxUndoDepth = 25

// snapshot is one turn's pre-mutation workspace, as a git tree id.
type snapshot struct {
	Tree  string `json:"tree"`
	Label string `json:"label"`
}

// checkpointStore holds the undo stack. All access is mutex-guarded: snapshots
// are taken from tool goroutines while /undo and finalize run on the UI loop,
// and the shadow repo has a single index file, so two git commands against it
// at once would corrupt each other's staging.
type checkpointStore struct {
	mu      sync.Mutex
	pending string // this turn's tree, "" until the turn mutates something
	stack   []snapshot
	off     bool   // shadow repo unusable; snapshots are skipped
	offWhy  string // and this is what to tell the user on /undo
}

// shadowRepoPath is the per-workspace shadow git dir. Derived from
// checkpointPath so the stack file and the objects it points at are always
// keyed by the same workspace — snapshots from one project can never rewind
// files in another.
func shadowRepoPath() string {
	return strings.TrimSuffix(checkpointPath(), ".json") + ".git"
}

// shadowExcludes keeps the user's own repo metadata out of the snapshot and
// stops the walk at dependency trees a project may not ignore itself. Without
// them a 5ms snapshot becomes a gigabyte one.
const shadowExcludes = "/.git/\nnode_modules/\n__pycache__/\n.venv/\nvenv/\n"

// shadowGitTimeout bounds one shadow-repo git command. The snapshot runs on
// the write path with m.ckpt.mu held, and the dispatcher's 90s tool deadline
// cannot cancel it — the Before hook takes no context — so an unbounded
// `git add -A` wedges the write that triggered it AND every later write queued
// on that mutex, each dying at 90s having written nothing. Bounded well under
// the tool deadline so a slow snapshot degrades to "undo off" (the sticky
// failure path below) instead of a hung tool call, and generous enough that a
// large repo's cold first snapshot — which hashes the whole work-tree — still
// succeeds. A var so the timeout test can shrink it.
//
// ponytail: a fixed ceiling, not a cancellable one. Plumb a context through
// Executor.Before if Ctrl-C ever needs to abort a snapshot mid-flight.
var shadowGitTimeout = 30 * time.Second

// shadowGit runs one git command against the shadow repo, from the workspace
// root so a "." pathspec means the whole work-tree. hooksPath is pinned inside
// the shadow dir — where there are no hooks — so a global core.hooksPath can't
// run the user's hooks on our bookkeeping.
func shadowGit(args ...string) ([]byte, error) {
	dir := shadowRepoPath()
	argv := append([]string{
		"--git-dir=" + dir,
		"--work-tree=" + workspaceRoot(),
		"-c", "core.hooksPath=" + filepath.Join(dir, "hooks"),
		// A nested repository in the workspace is staged as a gitlink, which
		// git narrates with a ten-line submodule hint on every snapshot.
		"-c", "advice.addEmbeddedRepo=false",
	}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), shadowGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = workspaceRoot()
	// Killing git does not release the output pipes if it forked a child that
	// inherited them; without a WaitDelay the copy goroutines keep Wait blocked
	// and the deadline above buys nothing.
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return out, fmt.Errorf("git %s timed out after %s", args[0], shadowGitTimeout)
		}
		return out, fmt.Errorf("git %s failed: %s", args[0], strings.TrimSpace(string(out)))
	}
	return out, nil
}

// ensureShadowLocked creates the shadow repo on first use. A failure is sticky:
// there is no point re-running a missing git binary once per tool call, and
// /undo needs a reason to show. Callers must hold m.ckpt.mu.
func (m *Model) ensureShadowLocked() bool {
	if m.ckpt.off {
		return false
	}
	dir := shadowRepoPath()
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return true
	}
	fail := func(why string) bool {
		m.ckpt.off, m.ckpt.offWhy = true, why
		return false
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fail("git is not installed")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err.Error())
	}
	if out, err := exec.Command("git", "init", "--bare", "--quiet", dir).CombinedOutput(); err != nil {
		return fail(strings.TrimSpace(string(out)))
	}
	if err := os.WriteFile(filepath.Join(dir, "info", "exclude"), []byte(shadowExcludes), 0o644); err != nil {
		return fail(err.Error())
	}
	return true
}

// writeWorkspaceTree stages the whole work-tree and writes it out as a tree
// object. Staging is not just a step on the way to the tree: it is what makes
// revert exact, because read-tree removes paths the index has and the target
// tree doesn't — that is how files created after a snapshot get deleted.
func writeWorkspaceTree() (string, error) {
	if _, err := shadowGit("add", "-A", "--", "."); err != nil {
		return "", err
	}
	out, err := shadowGit("write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// checkpointBeforeCall returns the executor Before hook that snapshots the
// workspace before a mutating tool call. Direct tool calls and sub-agent runs
// share it, so delegated writes land in the parent turn's snapshot and one
// /undo rewinds the whole delegation.
func (m *Model) checkpointBeforeCall() func(tools.ToolCall) {
	return func(call tools.ToolCall) {
		if paths := tools.MutatedPaths(call.Function.Name, call.Function.Arguments); len(paths) > 0 {
			m.snapshotBeforeMutate()
		}
	}
}

// snapshotBeforeMutate records the workspace as it stands before the turn's
// first mutating tool call. Later calls in the same turn are no-ops — the point
// is the pre-turn state — which is also why the old first-version-wins keying
// is gone: there are no per-path entries left to collide. Safe to call from a
// tool goroutine; a read-only turn never pays for it.
func (m *Model) snapshotBeforeMutate() {
	m.ckpt.mu.Lock()
	defer m.ckpt.mu.Unlock()
	if m.ckpt.pending != "" || !m.ensureShadowLocked() {
		return
	}
	tree, err := writeWorkspaceTree()
	if err != nil {
		m.ckpt.off, m.ckpt.offWhy = true, err.Error()
		return
	}
	m.ckpt.pending = tree
}

// finalizeCheckpoint pushes this turn's snapshot onto the undo stack. Called at
// turn end. The push is a no-op when nothing was mutated, but the turn-end
// auto-save runs either way.
func (m *Model) finalizeCheckpoint(label string) {
	m.ckpt.mu.Lock()
	if m.ckpt.pending != "" {
		m.ckpt.stack = append(m.ckpt.stack, snapshot{Tree: m.ckpt.pending, Label: label})
		if len(m.ckpt.stack) > maxUndoDepth {
			m.ckpt.stack = m.ckpt.stack[len(m.ckpt.stack)-maxUndoDepth:]
		}
		m.ckpt.pending = ""
		m.persistCheckpointsLocked()
	}
	m.ckpt.mu.Unlock()
	m.autosaveSession()
}

// persistCheckpointsLocked writes the undo stack — a few hundred bytes of tree
// ids — to its per-workspace file so /undo survives a restart. The contents
// those ids name are already durable in the shadow repo. Callers must hold
// m.ckpt.mu.
//
// ponytail: the shadow repo is never pruned. Git dedups blobs, so it grows with
// churn rather than with repo size; add a `git gc --prune` on trim if a
// long-lived workspace ever makes it hurt.
func (m *Model) persistCheckpointsLocked() {
	if !sessionPersist.Load() {
		return
	}
	data, err := json.Marshal(m.ckpt.stack)
	if err != nil {
		return
	}
	_ = writeFileAtomic(checkpointPath(), data, 0o644)
}

// loadPersistedCheckpoints restores the stack written by a previous process.
// Called on resume; a missing or corrupt file just means an empty stack. Files
// left by the pre-git-backed format decode into entries with no tree id — they
// carried inline file contents instead — and are dropped rather than replayed
// as a revert to nothing.
func (m *Model) loadPersistedCheckpoints() {
	data, err := os.ReadFile(checkpointPath())
	if err != nil {
		return
	}
	var stack []snapshot
	if err := json.Unmarshal(data, &stack); err != nil {
		return
	}
	kept := make([]snapshot, 0, len(stack))
	for _, s := range stack {
		if s.Tree != "" {
			kept = append(kept, s)
		}
	}
	if len(kept) > maxUndoDepth {
		kept = kept[len(kept)-maxUndoDepth:]
	}
	m.ckpt.mu.Lock()
	m.ckpt.stack = kept
	m.ckpt.mu.Unlock()
}

// noteUndoToModel tells the model which files were rolled back. Its own tool
// results from the undone turn are still in context ("edited main.go: replaced
// 1 occurrence(s)", the new hash, the diff) and now describe a file state that
// no longer exists; without this it keeps reasoning from them, and edit_file's
// fuzzy tier will match old_string against the reverted file at 0.85 similarity
// rather than failing clean.
//
// Slash commands are not queued while streaming (see updateChatKey), so /undo
// can land mid-batch, with the assistant's tool_calls message already in history
// and its results still pending. Appending there would splice a user turn
// between a tool_calls message and its results — the exact hazard advisory()'s
// doc comment warns about. So the message waits for the batch to finish, which
// is where advisory() says callers should append it.
func (m *Model) noteUndoToModel(touched []string) {
	if len(touched) == 0 {
		return
	}
	text := fmt.Sprintf("[UNDO] The user rolled back the last turn's file changes. These files were reverted to their state before that turn: %s. Your earlier edits to them no longer exist, and any file contents, hashes, or diffs you reported for them are stale. Re-read a file with read_file before editing or reasoning about it again.", strings.Join(touched, ", "))
	if m.pending != nil {
		m.deferredAdvisory = text
		return
	}
	m.history = append(m.history, advisory(text))
	// The popped checkpoint stack is already persisted; without this the
	// advisory is the one part of the undo a resumed session would lose,
	// putting the restored context right back at the defect this fixes.
	m.autosaveSession()
}

// flushDeferredAdvisory appends an advisory that was held back while a tool
// batch was in flight. Called once the batch's results are in history.
func (m *Model) flushDeferredAdvisory() {
	if m.deferredAdvisory == "" {
		return
	}
	m.history = append(m.history, advisory(m.deferredAdvisory))
	m.deferredAdvisory = ""
	m.autosaveSession()
}

// undoLast puts the work-tree back to the most recent snapshot. Returns a human
// summary and the absolute paths it touched (so the caller can refresh the RAG
// index and tell the model). Nothing is popped if the revert fails: a stack
// entry the user can retry beats one silently spent on an error.
func (m *Model) undoLast() (string, []string) {
	m.ckpt.mu.Lock()
	defer m.ckpt.mu.Unlock()
	if m.ckpt.off {
		return "can't undo — snapshots are off: " + m.ckpt.offWhy, nil
	}
	if len(m.ckpt.stack) == 0 {
		return "nothing to undo", nil
	}
	cp := m.ckpt.stack[len(m.ckpt.stack)-1]

	// Stage the current state first. It tells read-tree which files the turn
	// created — in the index, absent from the target tree — so they get
	// removed, and the blobs it writes leave the state being discarded
	// recoverable from the shadow repo instead of gone.
	if _, err := writeWorkspaceTree(); err != nil {
		return "can't undo — " + err.Error(), nil
	}
	// --no-renames keeps this to status/path pairs; rename detection would emit
	// a three-field record and desync the walk below.
	changed, err := shadowGit("diff", "--name-status", "--no-renames", "-z", "--cached", cp.Tree)
	if err != nil {
		return "can't undo — " + err.Error(), nil
	}
	if _, err := shadowGit("read-tree", "-u", "--reset", cp.Tree); err != nil {
		return "can't undo — " + err.Error(), nil
	}

	m.ckpt.stack = m.ckpt.stack[:len(m.ckpt.stack)-1]
	m.persistCheckpointsLocked()

	root := workspaceRoot()
	restored, deleted := 0, 0
	var touched []string
	fields := strings.Split(strings.Trim(string(changed), "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		// The diff runs tree → index, so "A" means the turn added the file and
		// the revert just removed it; everything else is a file put back.
		if strings.HasPrefix(fields[i], "A") {
			deleted++
		} else {
			restored++
		}
		touched = append(touched, filepath.Join(root, fields[i+1]))
	}
	return fmt.Sprintf("undid %q: %d file(s) restored, %d removed", cp.Label, restored, deleted), touched
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Stale-edit guard: refuse to mutate a file that changed on disk since the
// model last saw it.
//
// The hazard is ordinary for a local agent — the user tweaks a file in their
// editor while the model thinks, or a checkout/pull rewrites it between tool
// calls. The model's old_string was copied from a read taken before that
// change, and applyEdit's tier-3 fuzzy matcher commits at 0.85 similarity, so
// a stale edit does not fail cleanly: it finds something close enough and
// silently overwrites the change. Exact-match-only agents get a failed edit
// here; ocode gets a plausible wrong one, which is worse.
//
// The ledger records the hash of every file a read tool opened and of every
// file a tool successfully mutated, so it always holds "what the model last
// knew this file to be". It lives in the tools layer — carried on the
// tool-call context — rather than in one UI, so the TUI session, a headless
// run, and every spawned sub-agent each get the same protection. Each
// execution context holds its OWN ledger: agents share the filesystem but not
// observations, matching the per-agent observed-state design this is ported
// from (deepseek-harness's fs-observation-policy).
//
// There is no false-positive path. The check fires only when the bytes on
// disk genuinely differ from the ones the model was shown, which is a true
// statement whoever changed them — including a git_checkout or git_pull the
// model itself ran, where re-reading first is also the right answer.

// observingTools are the read tools whose successful call records a ledger
// observation. grep and find_files are excluded on purpose: they surface
// fragments, not the file's contents, so they can't anchor an edit baseline.
// A mutating tool's own internal read (edit_file loads the file to apply the
// edit) is NOT an observation either — only an explicit read call is.
var observingTools = map[string]bool{
	"read_file": true, "list_directory": true, "file_info": true,
}

// readFirstTools rewrite an existing file from content the model supplies, so
// the model must have seen that content first. Without this the 0.85 fuzzy
// tier lets an old_string recalled from training data, or guessed, land on the
// closest-looking lines of a file the model never opened. write_file is here
// because overwriting an unread file discards whatever was in it. Creating a
// file that does not exist yet is always allowed.
var readFirstTools = map[string]bool{
	"edit_file": true, "multi_edit": true, "write_file": true,
}

// FreshnessLedger maps clean path -> the file's hash when this agent last saw
// it. Session/run-scoped, NOT per-turn: editing between turns from a read
// taken in an earlier turn is the more common version of this mistake, not
// the rarer one. Thread-safe: a batch of tool calls runs on parallel
// goroutines against the same ledger.
type FreshnessLedger struct {
	mu     sync.Mutex
	hashes map[string]string
	// read holds paths whose contents the model has actually seen: a
	// read_file, an @-mention attachment, or its own write. list_directory and
	// file_info stamp a hash but show no contents, so they don't count.
	read map[string]bool
}

func NewFreshnessLedger() *FreshnessLedger {
	return &FreshnessLedger{hashes: map[string]string{}, read: map[string]bool{}}
}

// ledgerKey resolves p the way the tools will, so "main.go", "./main.go" and
// its absolute path share one entry.
func ledgerKey(p string) string {
	abs := expandHome(p)
	if !filepath.IsAbs(abs) {
		if cwd, err := os.Getwd(); err == nil {
			abs = filepath.Join(cwd, abs)
		}
	}
	return resolveJailPath(filepath.Clean(abs))
}

// Observe stamps the current on-disk hash of a path a read tool just opened.
// Called after the read succeeded. An unreadable path drops its baseline
// instead: with nothing to compare against later the guard stays silent, and
// a recreated file is not compared against a hash for bytes that are gone.
func (l *FreshnessLedger) Observe(path string) {
	clean := ledgerKey(path)
	hash, err := FileHash(clean)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		delete(l.hashes, clean)
		return
	}
	l.hashes[clean] = hash
}

// ObserveRead is Observe for a call that showed the model the file's
// contents, which is what satisfies the read-before-edit rule.
func (l *FreshnessLedger) ObserveRead(path string) {
	l.Observe(path)
	key := ledgerKey(path)
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.hashes[key]; ok {
		l.read[key] = true
	}
}

// RecordMutation re-stamps paths a tool just wrote. Without this the model's
// own successful edit would look like third-party drift on its next edit to
// the same file. The model wrote those bytes, so they count as read.
func (l *FreshnessLedger) RecordMutation(paths []string) {
	for _, p := range paths {
		l.ObserveRead(p)
	}
}

// CheckMutation refuses a mutating call against a file whose contents changed
// since the model last saw them. Returns nil to allow the call.
//
// The readFirstTools also refuse an existing file the model never read. A
// path that does not exist yet has nothing to read, so creating files is
// never blocked. Other mutators (move, copy, delete, append) don't rewrite
// content from the model's memory and are only drift-checked.
func (l *FreshnessLedger) CheckMutation(tool string, paths []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(paths) == 0 {
		return nil
	}
	for _, p := range paths {
		clean := ledgerKey(p)
		if readFirstTools[tool] && !l.read[clean] {
			if info, err := os.Stat(clean); err == nil && !info.IsDir() {
				return &UnreadFileError{Path: p, Tool: tool}
			}
		}
		known, seen := l.hashes[clean]
		if !seen {
			continue
		}
		current, err := FileHash(clean)
		if err != nil || current == known {
			continue
		}
		// Drop the stale baseline so the model is told once. It has to re-read
		// to proceed, and that read re-stamps the hash; without this a model
		// that ignores the message would be refused forever with the same text.
		delete(l.hashes, clean)
		delete(l.read, clean)
		return &StaleFileError{Path: clean, Tool: tool}
	}
	return nil
}

// StaleFileError is the model-coaching refusal produced by CheckMutation. It
// is a distinct type so result formatting (RepairHint) passes the message
// through verbatim instead of appending argument-repair advice.
type StaleFileError struct {
	Path string
	Tool string
}

func (e *StaleFileError) Error() string {
	return fmt.Sprintf("%s changed on disk since you read it — someone else edited it, or a checkout/pull rewrote it. Your copy is stale, so %s could silently overwrite that change. Read the file again and redo the edit against its current contents.", e.Path, e.Tool)
}

// UnreadFileError refuses a content rewrite of a file the model has not read
// in this session. Like StaleFileError it passes through RepairHint verbatim.
type UnreadFileError struct {
	Path string
	Tool string
}

func (e *UnreadFileError) Error() string {
	return fmt.Sprintf("%s has not been read in this session. Call read_file on it first, then redo %s against its actual contents. Edits built from memory or guesses can land on the wrong lines.", e.Path, e.Tool)
}

// freshnessCtxKey carries the execution context's ledger to the registry.
type freshnessCtxKey struct{}

// WithFreshnessLedger returns a context whose tool calls are guarded by l.
// The TUI installs one ledger per session; agent.Run installs a fresh one per
// run, so a headless run and each sub-agent observe independently of the
// parent they share the filesystem with.
func WithFreshnessLedger(ctx context.Context, l *FreshnessLedger) context.Context {
	return context.WithValue(ctx, freshnessCtxKey{}, l)
}

// FreshnessLedgerFrom returns the ledger guarding ctx, or nil when the caller
// installed none (direct handler invocations in tests, embedders). A nil
// ledger disables the guard, preserving the pre-ledger behavior of that path.
func FreshnessLedgerFrom(ctx context.Context) *FreshnessLedger {
	l, _ := ctx.Value(freshnessCtxKey{}).(*FreshnessLedger)
	return l
}

// observedPath extracts the path a read tool opened, or "" when the call
// named none (list_directory defaults to "."; matching the TUI ledger's rule,
// only an explicitly named path anchors a baseline).
func observedPath(raw json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return ""
	}
	return a.Path
}

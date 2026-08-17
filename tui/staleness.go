package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/javanhut/ollama_code/tools"
)

// Stale-edit guard: refuse to mutate a file that changed on disk since the
// model last saw it.
//
// The hazard is ordinary in a local TUI — you watch the agent work and tweak a
// file in your editor while it thinks. The model's old_string was copied from a
// read taken before that tweak, and applyEdit's tier-3 fuzzy matcher commits at
// 0.85 similarity, so a stale edit does not fail cleanly: it finds something
// close enough and silently overwrites your change. Exact-match-only agents get
// a failed edit here; ocode gets a plausible wrong one, which is worse.
//
// The ledger records the hash of every file a read tool opened and of every file
// a tool successfully mutated, so it always holds "what the model last knew this
// file to be". Deliberately session-scoped rather than per-turn, unlike the
// re-read ledger next to it: editing between turns from a read taken in an
// earlier turn is the more common version of this mistake, not the rarer one.
//
// There is no false-positive path. The check fires only when the bytes on disk
// genuinely differ from the ones the model was shown, which is a true statement
// whoever changed them — including a git_checkout or git_pull the model itself
// ran, where re-reading first is also the right answer.

// recordReadHashes stamps the current on-disk hash of every path a read tool
// just opened. Called after the batch's results are in, so the file has actually
// been read. Unreadable paths are skipped: with no baseline there is nothing to
// compare against later, and the guard stays silent.
func (m *Model) recordReadHashes(calls []tools.ToolCall) {
	for _, call := range calls {
		key, ok := readTargetKey(call)
		if !ok {
			continue
		}
		_, path, found := strings.Cut(key, "\x01")
		if !found {
			continue
		}
		m.rememberFileHash(path)
	}
}

// rememberFileHash records what a path currently holds, dropping the entry when
// the file cannot be hashed (deleted, or never existed) so a later recreate is
// not compared against a hash for different bytes.
func (m *Model) rememberFileHash(path string) {
	clean := filepath.Clean(path)
	if m.readHashes == nil {
		m.readHashes = map[string]string{}
	}
	hash, err := tools.FileHash(clean)
	if err != nil {
		delete(m.readHashes, clean)
		return
	}
	m.readHashes[clean] = hash
}

// rememberMutatedHashes re-stamps paths a tool just wrote. Without this the
// model's own successful edit would look like third-party drift on its next
// edit to the same file.
func (m *Model) rememberMutatedHashes(paths []string) {
	for _, p := range paths {
		m.rememberFileHash(p)
	}
}

// requireFreshRead refuses a mutating call against a file whose contents changed
// since the model last saw them. Returns "" to allow the call.
//
// Sits beside requireReadBeforeEdit in the dispatch preflight and answers a
// different question: that one asks whether the model looked at all, this one
// whether what it looked at is still true.
func (m *Model) requireFreshRead(name string, paths []string) string {
	if len(paths) == 0 || len(m.readHashes) == 0 {
		return ""
	}
	for _, p := range paths {
		clean := filepath.Clean(p)
		known, seen := m.readHashes[clean]
		if !seen {
			continue // never read it; requireReadBeforeEdit owns that question
		}
		current, err := tools.FileHash(clean)
		if err != nil || current == known {
			continue
		}
		// Drop the stale baseline so the model is told once. It has to re-read to
		// proceed, and that read re-stamps the hash; without this a model that
		// ignores the message would be refused forever with the same text.
		delete(m.readHashes, clean)
		return fmt.Sprintf("error: %s changed on disk since you read it — someone else edited it, or a checkout/pull rewrote it. Your copy is stale, so %s could silently overwrite that change. Read the file again and redo the edit against its current contents.", clean, name)
	}
	return ""
}

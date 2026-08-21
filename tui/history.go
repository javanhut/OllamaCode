package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Persistent prompt history and the draft stash (ported from opencode). Both
// are global — one file per user, not per session or workspace: history.json
// backs up/down recall, stash.jsonl is the /stash stack. Writes go through
// writeFileAtomic (temp + rename) and are best-effort everywhere: a failed
// save must never break input handling.

const (
	// historyCap bounds the recall list; the oldest entries drop off.
	historyCap = 200
	// stashCap bounds the /stash stack; the oldest entries drop off.
	stashCap = 50
)

// historyPath / stashPath live beside the auto-save in the state dir, so the
// session.SetDirForTesting redirect covers them in tests.
func historyPath() string { return filepath.Join(stateDir(), "history.json") }
func stashPath() string   { return filepath.Join(stateDir(), "stash.jsonl") }

// stashEntry is one line of stash.jsonl.
type stashEntry struct {
	Text string `json:"text"`
	At   int64  `json:"at"`
}

// appendHistory adds a submitted prompt to the recall list: consecutive
// duplicates collapse (re-running the same prompt shouldn't make recall step
// over it twice) and the list is capped most-recent-last.
func appendHistory(history []string, value string) []string {
	if strings.TrimSpace(value) == "" {
		return history
	}
	if n := len(history); n > 0 && history[n-1] == value {
		return history
	}
	history = append(history, value)
	if len(history) > historyCap {
		history = history[len(history)-historyCap:]
	}
	return history
}

// loadHistory reads history.json, tolerating a missing or corrupt file (both
// just start empty). Entries are normalized the same way appendHistory writes
// them, so a hand-edited file can't smuggle empties or dupes into recall.
func loadHistory() []string {
	data, err := os.ReadFile(historyPath())
	if err != nil {
		return nil
	}
	var raw []string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	var history []string
	for _, v := range raw {
		history = appendHistory(history, v)
	}
	return history
}

// saveHistory rewrites history.json whole — at 200 short entries that is
// cheap, and the atomic write means a crash mid-save leaves the old file.
func saveHistory(history []string) error {
	data, err := json.Marshal(history)
	if err != nil {
		return err
	}
	return writeFileAtomic(historyPath(), data, 0o644)
}

// persistHistory is submit's save hook, gated like the session auto-save so
// tests driving a bare Model never touch the real state dir.
func (m *Model) persistHistory() {
	if !sessionPersist.Load() {
		return
	}
	_ = saveHistory(m.userHistory)
}

// loadStash reads stash.jsonl, skipping corrupt or empty lines rather than
// failing the whole stack; a missing file is an empty stash.
func loadStash() []stashEntry {
	data, err := os.ReadFile(stashPath())
	if err != nil {
		return nil
	}
	var entries []stashEntry
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var e stashEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil || strings.TrimSpace(e.Text) == "" {
			continue
		}
		entries = append(entries, e)
	}
	return entries
}

// saveStash rewrites stash.jsonl whole, one JSON object per line. Multi-line
// drafts round-trip because JSON escapes the newlines inside the string.
func saveStash(entries []stashEntry) error {
	var b strings.Builder
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return writeFileAtomic(stashPath(), []byte(b.String()), 0o644)
}

// pushStash adds a draft to the stack, dropping the oldest beyond the cap.
func pushStash(entries []stashEntry, text string, at time.Time) []stashEntry {
	entries = append(entries, stashEntry{Text: text, At: at.Unix()})
	if len(entries) > stashCap {
		entries = entries[len(entries)-stashCap:]
	}
	return entries
}

// stashCommand: /stash — park the current draft on the stack and clear the
// input, so something urgent can go first without losing a half-typed
// message. Unlike the other /-commands the dispatch must not reset the input
// first: the draft is the payload.
func (m *Model) stashCommand() {
	value := strings.TrimRight(m.input.Value(), "\n")
	if strings.TrimSpace(value) == "" {
		m.toast = "nothing to stash"
		return
	}
	entries := pushStash(loadStash(), value, time.Now())
	if err := saveStash(entries); err != nil {
		m.toast = "stash failed: " + err.Error()
		return
	}
	m.input.Reset()
	m.input.SetHeight(minInputLines)
	m.layout()
	m.toast = fmt.Sprintf("stashed draft (%d in stash)", len(entries))
}

// unstashCommand: /unstash — pop the most recent stash entry back into the
// input. The restore path is the same one history recall uses: SetValue
// keeps embedded newlines, so a multi-line draft comes back as typed.
func (m *Model) unstashCommand() {
	entries := loadStash()
	if len(entries) == 0 {
		m.toast = "stash is empty"
		return
	}
	last := entries[len(entries)-1]
	if err := saveStash(entries[:len(entries)-1]); err != nil {
		m.toast = "unstash failed: " + err.Error()
		return
	}
	m.input.SetValue(last.Text)
	m.input.CursorEnd()
	m.layout()
	m.toast = "restored stashed draft"
}

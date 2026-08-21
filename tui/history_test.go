package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"

	"github.com/javanhut/ollama_code/internal/session"
)

// withStateDir redirects the state dir (history.json and stash.jsonl both
// derive from session.Dir) into a temp dir. Not parallel-safe: the session
// dir override is a package global.
func withStateDir(t *testing.T) {
	t.Helper()
	restore := session.SetDirForTesting(filepath.Join(t.TempDir(), "sessions"))
	t.Cleanup(restore)
}

// TestHistoryRoundTripWithCap: entries survive a save/load cycle, capped at
// historyCap with the oldest dropped and order most-recent-last.
func TestHistoryRoundTripWithCap(t *testing.T) {
	withStateDir(t)

	var history []string
	for i := range historyCap + 10 {
		history = appendHistory(history, fmt.Sprintf("prompt %d", i))
	}
	if err := saveHistory(history); err != nil {
		t.Fatal(err)
	}

	got := loadHistory()
	if len(got) != historyCap {
		t.Fatalf("loaded %d entries, want cap %d", len(got), historyCap)
	}
	if got[0] != "prompt 10" || got[len(got)-1] != fmt.Sprintf("prompt %d", historyCap+9) {
		t.Fatalf("oldest entries were not dropped: %q … %q", got[0], got[len(got)-1])
	}
}

// TestHistoryDedupeAndEmpty: consecutive duplicates collapse and empty or
// whitespace-only entries never enter the list; a repeat separated by another
// prompt is a new entry.
func TestHistoryDedupeAndEmpty(t *testing.T) {
	var history []string
	for _, v := range []string{"a", "a", "", "   ", "b", "a"} {
		history = appendHistory(history, v)
	}
	want := []string{"a", "b", "a"}
	if len(history) != len(want) {
		t.Fatalf("history = %v, want %v", history, want)
	}
	for i := range want {
		if history[i] != want[i] {
			t.Fatalf("history = %v, want %v", history, want)
		}
	}
}

// TestHistoryLoadToleratesMissingAndCorrupt: no file and a garbage file both
// load as an empty history instead of an error.
func TestHistoryLoadToleratesMissingAndCorrupt(t *testing.T) {
	withStateDir(t)

	if got := loadHistory(); got != nil {
		t.Fatalf("missing file loaded %v, want empty", got)
	}
	if err := os.WriteFile(historyPath(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadHistory(); got != nil {
		t.Fatalf("corrupt file loaded %v, want empty", got)
	}
}

// TestStashPushPopWithCap: the stash is a LIFO stack capped at stashCap, with
// the oldest entries dropped.
func TestStashPushPopWithCap(t *testing.T) {
	withStateDir(t)

	now := time.Now()
	var entries []stashEntry
	for i := range stashCap + 5 {
		entries = pushStash(entries, fmt.Sprintf("draft %d", i), now)
	}
	if err := saveStash(entries); err != nil {
		t.Fatal(err)
	}

	got := loadStash()
	if len(got) != stashCap {
		t.Fatalf("loaded %d entries, want cap %d", len(got), stashCap)
	}
	if got[0].Text != "draft 5" {
		t.Fatalf("oldest entries were not dropped: first is %q", got[0].Text)
	}
	if last := got[len(got)-1]; last.Text != fmt.Sprintf("draft %d", stashCap+4) || last.At != now.Unix() {
		t.Fatalf("top of stack drifted: %+v", last)
	}
}

// TestStashMultiLineRoundTrip: a draft with embedded newlines survives the
// JSONL file intact — each entry must stay exactly one line on disk.
func TestStashMultiLineRoundTrip(t *testing.T) {
	withStateDir(t)

	draft := "first line\nsecond line\n\nfourth line"
	if err := saveStash(pushStash(nil, draft, time.Now())); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stashPath())
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n"); len(lines) != 1 {
		t.Fatalf("multi-line draft split across %d file lines", len(lines))
	}
	got := loadStash()
	if len(got) != 1 || got[0].Text != draft {
		t.Fatalf("round trip drifted: %+v", got)
	}
}

// TestStashLoadSkipsCorruptLines: a garbage line is dropped without failing
// the entries around it; a missing file is an empty stash.
func TestStashLoadSkipsCorruptLines(t *testing.T) {
	withStateDir(t)

	if got := loadStash(); got != nil {
		t.Fatalf("missing file loaded %v, want empty", got)
	}
	content := "{\"text\":\"good\",\"at\":1}\nnot json\n{\"text\":\"\",\"at\":2}\n{\"text\":\"also good\",\"at\":3}\n"
	if err := os.WriteFile(stashPath(), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadStash()
	if len(got) != 2 || got[0].Text != "good" || got[1].Text != "also good" {
		t.Fatalf("corrupt lines not skipped: %+v", got)
	}
}

// TestStashCommand: /stash parks the draft and clears the input; /unstash
// pops it back. An empty input stashes nothing.
func TestStashCommand(t *testing.T) {
	withStateDir(t)

	m := &Model{input: textarea.New()}
	m.stashCommand()
	if m.toast != "nothing to stash" {
		t.Fatalf("empty stash toast = %q", m.toast)
	}

	draft := "half-typed thought\nacross two lines"
	m.input.SetValue(draft)
	m.stashCommand()
	if m.input.Value() != "" {
		t.Fatalf("input not cleared: %q", m.input.Value())
	}
	if m.toast != "stashed draft (1 in stash)" {
		t.Fatalf("toast = %q", m.toast)
	}

	m.unstashCommand()
	if m.input.Value() != draft {
		t.Fatalf("draft not restored: %q", m.input.Value())
	}
	if got := loadStash(); len(got) != 0 {
		t.Fatalf("stash entry not popped: %+v", got)
	}

	m.input.Reset()
	m.unstashCommand()
	if m.toast != "stash is empty" {
		t.Fatalf("empty unstash toast = %q", m.toast)
	}
}

// TestSubmitPersistsHistory: a submit appends to the recall list and rewrites
// history.json; the gate keeps a bare Model from touching disk.
func TestSubmitPersistsHistory(t *testing.T) {
	withStateDir(t)

	m := &Model{}
	m.userHistory = appendHistory(m.userHistory, "first")
	m.persistHistory()
	if _, err := os.Stat(historyPath()); !os.IsNotExist(err) {
		t.Fatal("history written with persistence disabled")
	}

	sessionPersist.Store(true)
	t.Cleanup(func() { sessionPersist.Store(false) })
	m.userHistory = appendHistory(m.userHistory, "second")
	m.persistHistory()

	got := loadHistory()
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("persisted history drifted: %v", got)
	}
}

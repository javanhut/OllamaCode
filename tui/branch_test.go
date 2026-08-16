package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/session"
)

// branchModel is a conversation of three user turns, each answered, with an
// advisory in the middle: the guards' own messages ride the user role and must
// not be counted as turns the user can rewind to.
func branchModel(t *testing.T) *Model {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := statusTestModel()
	m.history = []api.Message{
		{Role: "user", Content: "first"},         // 0
		{Role: "assistant", Content: "answer 1"}, // 1
		{Role: "user", Content: "second"},        // 2
		advisory("[REPEATING ACTION] stop that"), // 3
		{Role: "assistant", Content: "answer 2"}, // 4
		{Role: "user", Content: "third"},         // 5
		{Role: "assistant", Content: "answer 3"}, // 6
	}
	return m
}

func TestUserTurnStartsIgnoresAdvisories(t *testing.T) {
	m := branchModel(t)
	got := m.userTurnStarts()
	want := []int{0, 2, 5}
	if len(got) != len(want) {
		t.Fatalf("turn starts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("turn starts = %v, want %v", got, want)
		}
	}
}

func TestRewindDropsTheLastTurn(t *testing.T) {
	defer session.SetDirForTesting(t.TempDir())()
	m := branchModel(t)

	m.rewindCommand("")

	if len(m.history) != 5 {
		t.Fatalf("history = %d messages, want 5", len(m.history))
	}
	if last := m.history[len(m.history)-1].Content; last != "answer 2" {
		t.Fatalf("rewound to %q, want the second turn's answer", last)
	}
	if !strings.Contains(m.toast, "files untouched") {
		t.Errorf("toast should say files are not reverted: %q", m.toast)
	}
}

// A rewind is never a one-way door: the discarded tail is forked to a session
// before the log is shortened.
func TestRewindBacksUpWhatItDrops(t *testing.T) {
	defer session.SetDirForTesting(t.TempDir())()
	m := branchModel(t)

	m.rewindCommand("2")

	if len(m.history) != 2 {
		t.Fatalf("history = %d messages, want 2", len(m.history))
	}
	names, err := session.List()
	if err != nil || len(names) == 0 {
		t.Fatalf("no backup session written: %v %v", names, err)
	}
	s, err := session.Load(names[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 7 {
		t.Fatalf("backup holds %d messages, want the whole conversation", len(s.Messages))
	}
}

func TestRewindClearsStateDerivedFromDroppedMessages(t *testing.T) {
	defer session.SetDirForTesting(t.TempDir())()
	m := branchModel(t)
	m.turnRecords = map[int]turnRecord{0: {total: time.Second}, 5: {total: time.Second}}
	m.totalTokens, m.lastPromptEval, m.prevPromptEval = 9000, 9000, 9000

	m.rewindCommand("1")

	if _, ok := m.turnRecords[5]; ok {
		t.Error("timing for a dropped turn survived")
	}
	if _, ok := m.turnRecords[0]; !ok {
		t.Error("timing for a kept turn was discarded")
	}
	if m.totalTokens != 0 || m.lastPromptEval != 0 {
		t.Error("token measurements describe a prompt that no longer exists")
	}
}

// Rewinding past the compaction boundary brings the archived messages back into
// view, so the summary standing in for them has to go or they are stated twice.
func TestRewindPastTheArchiveBoundaryDropsTheSummary(t *testing.T) {
	defer session.SetDirForTesting(t.TempDir())()
	m := branchModel(t)
	m.archivedThrough, m.prunedThrough = 4, 4
	m.archiveSummary = "the earlier conversation"

	m.rewindCommand("2") // cut at index 2, before the boundary

	if m.archiveSummary != "" {
		t.Errorf("archive summary survived a rewind past it: %q", m.archiveSummary)
	}
	if m.archivedThrough != 0 || m.prunedThrough != 0 {
		t.Errorf("boundaries not retired: archived=%d pruned=%d", m.archivedThrough, m.prunedThrough)
	}
	if len(m.deriveModelMessages()) != 2 {
		t.Errorf("model view = %d messages, want the whole rewound log", len(m.deriveModelMessages()))
	}
}

func TestForkLeavesTheLiveConversationAlone(t *testing.T) {
	defer session.SetDirForTesting(t.TempDir())()
	m := branchModel(t)

	m.forkCommand("2 the-branch")

	if len(m.history) != 7 {
		t.Fatalf("fork mutated the live conversation: %d messages", len(m.history))
	}
	s, err := session.Load("the-branch")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 {
		t.Fatalf("branch holds %d messages, want 2 (cut at the second turn)", len(s.Messages))
	}
	if !strings.Contains(m.toast, "the-branch") {
		t.Errorf("toast should name the branch: %q", m.toast)
	}
}

func TestParseTurnArg(t *testing.T) {
	for _, tc := range []struct {
		args string
		n    int
		name string
	}{
		{"", 0, ""},
		{"3", 3, ""},
		{"3 my-branch", 3, "my-branch"},
		{"my-branch", 0, "my-branch"},
		{"2nd-attempt", 0, "2nd-attempt"}, // a name that starts with a digit is still a name
	} {
		n, name := parseTurnArg(tc.args)
		if n != tc.n || name != tc.name {
			t.Errorf("parseTurnArg(%q) = (%d, %q), want (%d, %q)", tc.args, n, name, tc.n, tc.name)
		}
	}
}

func TestTurnCutRejectsMoreTurnsThanExist(t *testing.T) {
	m := branchModel(t)
	if _, err := m.turnCut(9); err == nil {
		t.Fatal("expected an error for a rewind past the start of the conversation")
	}
}

// "/save myname" and "/load myname" were `case` arms in a switch on the whole
// input line, so only the bare word ever matched: the name was never parsed,
// and the line fell past the switch to the model as an ordinary chat message.
// /load was therefore unusable — the only reachable branch printed its usage.
func TestSaveAndLoadTakeANameFromTheCommandLine(t *testing.T) {
	defer session.SetDirForTesting(t.TempDir())()
	m := branchModel(t)
	m.modelName = "qwen2.5-coder:7b"

	if _, cmd := m.slashInput(t, "/save my-work"); cmd != nil {
		t.Fatal("/save with a name was not handled")
	}
	if !strings.Contains(m.toast, "my-work") {
		t.Fatalf("save toast = %q, want the name", m.toast)
	}
	if _, err := session.Load("my-work"); err != nil {
		t.Fatalf("session was not written under the given name: %v", err)
	}

	m.history = nil
	if _, cmd := m.slashInput(t, "/load my-work"); cmd != nil {
		t.Fatal("/load with a name was not handled")
	}
	if len(m.history) != 7 {
		t.Fatalf("loaded %d messages, want the saved conversation", len(m.history))
	}
	if !strings.Contains(m.toast, "loaded session 'my-work'") {
		t.Fatalf("load toast = %q", m.toast)
	}
}

// slashInput types val and presses enter, the way the command actually reaches
// the dispatcher — the bug was in that routing, so a test calling the handler
// directly would have passed against the broken build.
func (m *Model) slashInput(t *testing.T, val string) (tea.Model, tea.Cmd) {
	t.Helper()
	m.state = stateChat
	m.input.SetValue(val)
	return m.updateChatKey(tea.KeyPressMsg{Code: tea.KeyEnter})
}

package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// ctrl+c during the compile check, retrieval or compaction stops the turn. It
// used to quit ocode, because only streaming counted as mid-turn.
func TestCtrlCInterruptsEveryPhase(t *testing.T) {
	for _, phase := range []string{"verifying", "compacting", "retrieving"} {
		t.Run(phase, func(t *testing.T) {
			m := statusTestModel()
			cancelled := false
			switch phase {
			case "verifying":
				m.phase, m.verifyCancel = phaseVerifying, func() { cancelled = true }
			case "compacting":
				m.compacting, m.compactCancel = true, func() { cancelled = true }
			case "retrieving":
				m.phase, cancelled = phaseRetrieving, true // nothing to cancel mid-call
			}
			_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			if isQuit(cmd) {
				t.Fatal("ctrl+c quit ocode mid-turn")
			}
			if m.turnActive() || !cancelled {
				t.Fatalf("turn still active=%v, work cancelled=%v", m.turnActive(), cancelled)
			}
		})
	}
}

func TestCtrlCQuitsWhenIdle(t *testing.T) {
	m := statusTestModel()
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); !isQuit(cmd) {
		t.Fatal("ctrl+c while idle should quit")
	}
}

func TestEscStopsTheCompileCheck(t *testing.T) {
	m := statusTestModel()
	cancelled := false
	m.phase, m.verifyCancel = phaseVerifying, func() { cancelled = true }
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !cancelled || (m.phase == phaseVerifying) {
		t.Fatalf("cancelled=%v verifying=%v", cancelled, (m.phase == phaseVerifying))
	}
}

// Results from work started before an interrupt are dropped. A late compile
// failure used to start a repair turn for a turn that no longer existed.
func TestStaleBackgroundResultsAreDropped(t *testing.T) {
	m := statusTestModel()
	epoch := m.workEpoch
	m.phase = phaseVerifying
	m.interruptTurn()
	before := len(m.history)

	_, cmd := m.Update(verifyDoneMsg{ok: false, label: "go build", output: "boom", epoch: epoch})
	if cmd != nil || len(m.history) != before || m.generating() {
		t.Fatalf("stale verify result acted: cmd=%v history %d->%d streaming=%v", cmd != nil, before, len(m.history), m.generating())
	}
	if _, cmd := m.Update(ragRetrievedMsg{query: "q", block: "b", epoch: epoch}); cmd != nil || m.generating() {
		t.Fatal("stale retrieval started a stream")
	}
	m.history = append(m.history, make([]api.Message, 8)...)
	m.Update(compactDoneMsg{summary: "old summary", index: 4, epoch: epoch})
	if m.archivedThrough != 0 || m.archiveSummary != "" {
		t.Fatal("stale compaction moved the archive boundary")
	}
}

func TestInterruptCancelsToolBatch(t *testing.T) {
	m := statusTestModel()
	m.startToolBatch(nil)
	ctx := m.pending.ctx
	m.interruptTurn()
	if ctx.Err() == nil {
		t.Fatal("running tool calls were not cancelled")
	}
}

// A failed compaction request clears the flag through compactDoneMsg. As a
// chatErrMsg it carried a stale turnGen, was dropped, and left compacting on.
func TestCompactionErrorClearsFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusBadRequest)
	}))
	defer server.Close()
	m := overflowTestModel(t)
	m.host.SetURI(server.URL)

	cmd := m.compactContext(true)
	if cmd == nil {
		t.Fatal("no compaction started")
	}
	m.turnGen++ // the turn moves on while the pass runs
	msg, ok := cmd().(compactDoneMsg)
	if !ok || !strings.Contains(msg.reason, "request failed") {
		t.Fatalf("got %#v, want a compactDoneMsg with the failure", msg)
	}
	m.Update(msg)
	if m.compacting {
		t.Fatal("compacting stuck on after a failed pass")
	}
}

// A message sent while a turn is in any phase waits in the queue. It used to
// queue only while streaming, so one typed during the compile check or a RAG
// lookup started a second turn beside the running one.
func TestMessageQueuesInEveryTurnPhase(t *testing.T) {
	for _, p := range []turnPhase{phaseRetrieving, phaseStreaming, phaseTools, phaseVerifying} {
		t.Run(p.String(), func(t *testing.T) {
			m := statusTestModel()
			m.modelName = "qwen3:8b"
			m.phase = p
			m.input.SetValue("one more thing")
			m.updateChatKey(tea.KeyPressMsg{Code: tea.KeyEnter})
			if len(m.queue) != 1 || m.phase != p {
				t.Fatalf("queue=%d phase=%s, want the message queued and the phase unchanged", len(m.queue), m.phase)
			}
		})
	}
}

func TestDreamWaitsForTheWholeTurn(t *testing.T) {
	for _, p := range []turnPhase{phaseRetrieving, phaseVerifying} {
		m := statusTestModel()
		m.phase = p
		if m.maybeDream() != nil {
			t.Fatalf("dream started during %s", p)
		}
	}
}

// Every phase, and a background compaction, is left cleanly by an interrupt:
// idle, nothing in flight, and ctrl+c quitting again afterwards.
func TestInterruptFromEveryPhaseReachesIdle(t *testing.T) {
	for _, p := range []turnPhase{phaseRetrieving, phaseStreaming, phaseTools, phaseVerifying} {
		for _, compacting := range []bool{false, true} {
			m := statusTestModel()
			m.phase, m.compacting = p, compacting
			if p == phaseTools {
				m.startToolBatch(nil)
			}
			m.interruptTurn()
			if m.turnActive() || m.pending != nil || m.stream != nil {
				t.Fatalf("%s (compacting=%v): still active after interrupt", p, compacting)
			}
			if _, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); !isQuit(cmd) {
				t.Fatalf("%s: ctrl+c should quit once idle", p)
			}
		}
	}
}

// Keys that arrive as an approval prompt opens are the tail of whatever the
// user was typing, not an answer: "now rewrite…" used to deny a write with its
// first letter.
func TestPromptIgnoresKeysDuringGrace(t *testing.T) {
	m := statusTestModel()
	m.startToolBatch([]tools.ToolCall{{Function: tools.ToolCallFunction{Name: "write_file", Arguments: []byte(`{"path":"a.txt","content":"x"}`)}}})
	m.showPrompt(statePermission)

	m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if m.state != statePermission || m.pending.started[0] {
		t.Fatal("a key typed as the prompt opened answered it")
	}

	// The user has read it and stopped typing.
	m.promptShownAt = time.Now().Add(-time.Second)
	m.promptKeyAt = time.Now().Add(-time.Second)
	m.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if m.state == statePermission {
		t.Fatal("a deliberate answer after the grace period was ignored")
	}
}

// Someone typing through the prompt without noticing it keeps it deaf until
// they pause, however long they type.
func TestPromptGraceExtendsWhileTyping(t *testing.T) {
	m := statusTestModel()
	m.startToolBatch([]tools.ToolCall{{Function: tools.ToolCallFunction{Name: "write_file", Arguments: []byte(`{"path":"a.txt","content":"x"}`)}}})
	m.showPrompt(statePermission)
	m.promptShownAt = time.Now().Add(-2 * promptGrace) // opened a while ago...
	m.promptKeyAt = time.Now()                         // ...but the user is still typing
	m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if m.state != statePermission || m.pending.started[0] {
		t.Fatal("a key mid-typing approved the prompt")
	}
	if !strings.Contains(m.toast, "prompt just opened") {
		t.Fatalf("toast = %q, want an explanation", m.toast)
	}
}

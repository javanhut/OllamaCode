package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

var errOverflow = errors.New(`unexpected status code: 400: {"error":{"code":"context_length_exceeded"}}`)

// overflowTestModel is statusTestModel with a history long enough to compact
// and nothing prunable in it, so compactContext takes the summarization path.
func overflowTestModel(t *testing.T) *Model {
	t.Helper()
	// logActivity writes the config file; keep it out of the real HOME.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	m := statusTestModel()
	m.contextLimit = 32768
	m.turnGen = 7
	m.streaming = true
	m.stream = &streamState{gen: 7, cancel: func() {}}
	for i := range 8 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		m.history = append(m.history, api.Message{Role: role, Content: strings.Repeat("conversation ", 200)})
	}
	return m
}

// drainCmd runs cmd and returns the messages it produced, flattening one batch.
func drainCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	raw := cmd()
	batch, ok := raw.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{raw}
	}
	out := make([]tea.Msg, 0, len(batch))
	for _, c := range batch {
		if c != nil {
			out = append(out, c())
		}
	}
	return out
}

func TestChatErrContextOverflowCompactsInsteadOfRetrying(t *testing.T) {
	m := overflowTestModel(t)

	_, cmd := m.Update(chatErrMsg{gen: 7, err: errOverflow})

	if m.streamRetries != 0 {
		t.Fatalf("overflow spent %d transient retries; it must not touch that budget", m.streamRetries)
	}
	if !m.compacting || !m.overflowRetried {
		t.Fatalf("overflow did not force a compaction (compacting=%v retried=%v)", m.compacting, m.overflowRetried)
	}
	if m.overflowErr == nil || m.overflowTokens == 0 {
		t.Fatalf("recovery state not recorded: err=%v tokens=%d", m.overflowErr, m.overflowTokens)
	}
	if cmd == nil {
		t.Fatal("expected the compaction command")
	}
	if m.lastError != "" {
		t.Fatalf("turn was killed instead of recovered: %q", m.lastError)
	}
}

func TestCompactDoneRetriesShrunkenRequest(t *testing.T) {
	m := overflowTestModel(t)
	m.overflowRetried = true
	m.overflowErr = errOverflow
	m.overflowTokens = m.requestEstimate()

	_, cmd := m.Update(compactDoneMsg{summary: "short", index: 4})

	msgs := drainCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("expected one follow-up message, got %#v", msgs)
	}
	if retry, ok := msgs[0].(retryStreamMsg); !ok || retry.gen != 7 {
		t.Fatalf("expected retryStreamMsg{gen:7}, got %#v", msgs[0])
	}
	if m.overflowErr != nil {
		t.Fatal("pending overflow error must be cleared once the retry is issued")
	}
}

func TestCompactDoneSurfacesOverflowWhenCompactionMadeNoProgress(t *testing.T) {
	m := overflowTestModel(t)
	m.overflowRetried = true
	m.overflowErr = errOverflow
	m.overflowTokens = m.requestEstimate()

	// index 0 drops nothing and the summary is bigger than what it replaced:
	// re-issuing this request would overflow identically.
	_, cmd := m.Update(compactDoneMsg{summary: strings.Repeat("x ", 4000), index: 0})

	msgs := drainCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("expected one follow-up message, got %#v", msgs)
	}
	failed, ok := msgs[0].(chatErrMsg)
	if !ok {
		t.Fatalf("expected the original error to be surfaced, got %#v", msgs[0])
	}
	if !errors.Is(failed.err, errOverflow) {
		t.Fatalf("surfaced the wrong error: %v", failed.err)
	}
}

func TestChatErrOverflowAfterRecoveryEndsTurn(t *testing.T) {
	m := overflowTestModel(t)
	m.overflowRetried = true // the one recovery this turn gets is already spent

	m.Update(chatErrMsg{gen: 7, err: errOverflow})

	if m.streamRetries != 0 {
		t.Fatalf("overflow spent %d transient retries", m.streamRetries)
	}
	if m.compacting {
		t.Fatal("a second forced compaction ran — recovery can loop")
	}
	if m.lastError == "" {
		t.Fatal("unrecoverable overflow must surface as the turn's error")
	}
}

// prunableOverflowModel has aged tool results in it, so pruning finds something
// to reclaim — the case where the recovery used to declare victory over a
// handful of tokens.
func prunableOverflowModel(t *testing.T) *Model {
	t.Helper()
	m := overflowTestModel(t)
	for range 12 {
		m.history = append(m.history, api.Message{Role: "assistant", ToolCalls: []tools.ToolCall{{
			Function: tools.ToolCallFunction{Name: "todo_write"},
		}}})
		m.history = append(m.history, api.Message{
			Role:     "tool",
			ToolName: "todo_write",
			Content:  `{"ok":true,"summary":"noted","evidence":["a line of evidence"],"hint":"a hint"}`,
		})
	}
	return m
}

// A token-trivial prune is not a recovery: the PROVIDER refused this request,
// so a prune that only clears our own threshold proves nothing. Re-sending a
// request a few tokens smaller spent the one allowed retry and killed the turn
// on the second 400, with the summarization never running.
func TestOverflowForcesSummarizationEvenWhenPruningFindsSomething(t *testing.T) {
	m := prunableOverflowModel(t)

	_, cmd := m.Update(chatErrMsg{gen: 7, err: errOverflow})

	if !m.compacting {
		t.Fatal("overflow accepted a prune instead of forcing summarization")
	}
	if cmd == nil {
		t.Fatal("expected the compaction command")
	}
	for _, msg := range drainCmd(cmd) {
		if _, isRetry := msg.(retryStreamMsg); isRetry {
			t.Fatal("retried before the summary landed")
		}
	}
}

// The proactive path must not do the mirror-image of that: a measured-pressure
// pass that finds one small prunable result used to zero the measurement and
// re-check with the char estimate, which by definition reads below the
// threshold — so every such pass cancelled itself, forever.
func TestPruneDoesNotCancelAMeasuredCompaction(t *testing.T) {
	m := prunableOverflowModel(t)
	m.observePromptEval(28000) // 85% of 32768; the estimate is nowhere near it
	m.observePromptEval(28000)
	if !m.shouldCompact() {
		t.Fatal("setup: measured pressure should have crossed the threshold")
	}

	cmd := m.compactContext(false)

	if cmd == nil || !m.compacting {
		t.Fatalf("a trivial prune cancelled the compaction (cmd=%v compacting=%v)", cmd != nil, m.compacting)
	}
}

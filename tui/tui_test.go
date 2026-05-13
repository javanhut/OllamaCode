package tui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/mcp"
)

func TestParseModeSwitchArgs(t *testing.T) {
	req, err := parseModeSwitchArgs(json.RawMessage(`{"mode":" WRITE ","reason":"approved"}`))
	if err != nil {
		t.Fatalf("parseModeSwitchArgs returned error: %v", err)
	}
	if req.target != WriteMode {
		t.Fatalf("expected write target, got %v", req.target)
	}
	if req.mode != "write" {
		t.Fatalf("expected normalized mode name, got %q", req.mode)
	}
	if req.reason != "approved" {
		t.Fatalf("expected trimmed reason, got %q", req.reason)
	}
}

func TestApplyModeTransitionAddsPlanSummaryWhenEnteringWrite(t *testing.T) {
	m := &Model{
		mode:  PlanMode,
		notes: &sessionNotes{text: "1. inspect parser\n2. patch transition"},
	}

	if !m.applyModeTransition(WriteMode, "plan approved") {
		t.Fatal("expected mode transition")
	}
	if m.mode != WriteMode {
		t.Fatalf("expected write mode, got %v", m.mode)
	}
	if len(m.history) != 1 {
		t.Fatalf("expected one history message, got %d", len(m.history))
	}
	if m.history[0].Role != "system" || !strings.Contains(m.history[0].Content, "Plan Summary from Session Notes") {
		t.Fatalf("expected plan summary system message, got %#v", m.history[0])
	}
}

func TestSwitchModeToolSequencesFollowingCallsAgainstNewMode(t *testing.T) {
	m := &Model{
		mode:       ExploreMode,
		state:      stateChat,
		tools:      mcp.NewRegistry(),
		notes:      &sessionNotes{},
		transcript: &strings.Builder{},
		streamBuf:  &strings.Builder{},
		mdCache:    map[string]string{},
		history: []api.Message{{
			Role: "assistant",
			ToolCalls: []mcp.ToolCall{
				{
					Function: mcp.ToolCallFunction{
						Name:      "switch_mode",
						Arguments: json.RawMessage(`{"mode":"write","reason":"ready to edit"}`),
					},
				},
				{
					Function: mcp.ToolCallFunction{
						Name:      "write_file",
						Arguments: json.RawMessage(`{"path":"example.txt","content":"hello"}`),
					},
				},
			},
		}},
	}
	m.tools.Register(m.switchModeTool())
	m.pending = &pendingBatch{
		calls:   m.history[0].ToolCalls,
		results: make([]api.Message, 2),
		started: make([]bool, 2),
	}

	cmd := m.processPendingTools()
	if cmd == nil {
		t.Fatal("expected switch_mode command")
	}
	if !m.pending.started[0] {
		t.Fatal("expected switch_mode to be started")
	}
	if m.pending.started[1] {
		t.Fatal("write_file should wait until switch_mode result is applied")
	}

	raw := cmd()
	if batch, ok := raw.(tea.BatchMsg); ok {
		if len(batch) != 1 {
			t.Fatalf("expected one batched command, got %d", len(batch))
		}
		raw = batch[0]()
	}
	msg, ok := raw.(toolResultMsg)
	if !ok {
		t.Fatalf("expected toolResultMsg, got %T", raw)
	}
	if msg.modeSwitch == nil {
		t.Fatal("expected mode switch request on tool result")
	}
	m.pending.results[msg.index] = msg.result
	m.pending.done++
	m.applyModeTransition(msg.modeSwitch.target, msg.modeSwitch.reason)

	cmd = m.processPendingTools()
	if cmd != nil {
		t.Fatalf("expected permission prompt, got command %T", cmd)
	}
	if m.pending.done != 1 {
		t.Fatalf("write_file should not be rejected before permission prompt; done=%d", m.pending.done)
	}
	if !m.pending.started[0] || m.pending.started[1] {
		t.Fatalf("unexpected pending started state: %#v", m.pending.started)
	}
	if m.state != statePermission {
		t.Fatalf("expected permission state, got %v", m.state)
	}
}

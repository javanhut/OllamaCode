package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
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
		mode:       PlanMode,
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
	if cmd != nil {
		t.Fatal("expected nil command as switch_mode is now permission-gated")
	}
	if m.state != statePermission {
		t.Fatalf("expected statePermission, got %v", m.state)
	}

	// Simulate user approval (pressing 'y')
	_, cmd = m.updatePermission(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if cmd == nil {
		t.Fatal("expected switch_mode command after user approval")
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

func TestSelectedTranscriptLineUsesSelectionRange(t *testing.T) {
	m := &Model{
		transcript: &strings.Builder{},
		sel:        selection{active: true, anchor: 3, cursor: 1},
	}
	m.transcript.WriteString("zero\none\ntwo\nthree\nfour")

	for _, line := range []int{1, 2, 3} {
		if !m.selectedTranscriptLine(line) {
			t.Fatalf("expected line %d to be selected", line)
		}
	}
	for _, line := range []int{0, 4} {
		if m.selectedTranscriptLine(line) {
			t.Fatalf("expected line %d not to be selected", line)
		}
	}
}

func TestTranscriptLineAtVisualOffsetAccountsForSoftWrap(t *testing.T) {
	m := &Model{
		transcript: &strings.Builder{},
	}
	m.transcript.WriteString("short\n1234567890abcdefghij\nlast")
	m.viewport.SetWidth(10)
	m.viewport.SoftWrap = true

	tests := map[int]int{
		0: 0,
		1: 1,
		2: 1,
		3: 2,
	}
	for offset, want := range tests {
		if got := m.transcriptLineAtVisualOffset(offset); got != want {
			t.Fatalf("offset %d: expected line %d, got %d", offset, want, got)
		}
	}
}

func TestIsExploreReadOnlyShell(t *testing.T) {
	allowed := []string{
		"ls -la",
		"cat README.md",
		"head -n 20 main.go",
		"grep -rn foo .",
		"rg --files",
		"find . -name '*.go'",
		"git status",
		"git log --oneline -n 5",
		"git diff HEAD~1",
		"go version",
		"go list ./...",
		"ls | wc -l",
		"cat file.txt | grep foo | sort | uniq",
		"ps aux 2>&1",
		"ls -la && pwd",
		"FOO=bar env",
		"cat 'has > in name.txt'",
	}
	for _, cmd := range allowed {
		ok, reason := isExploreReadOnlyShell(cmd)
		if !ok {
			t.Errorf("expected %q to be allowed; rejected: %s", cmd, reason)
		}
	}

	blocked := []string{
		"rm -rf /tmp/foo",
		"mv a b",
		"echo hi > out.txt",
		"cat a >> b",
		"sed -i 's/a/b/' file",
		"sudo cat /etc/shadow",
		"git push",
		"git commit -m oops",
		"git checkout main",
		"go build ./...",
		"go run main.go",
		"$(rm -rf /)",
		"`whoami`",
		"ls; rm foo",
		"ls && rm foo",
		"npm install",
		"curl https://example.com",
	}
	for _, cmd := range blocked {
		ok, _ := isExploreReadOnlyShell(cmd)
		if ok {
			t.Errorf("expected %q to be blocked, but it was allowed", cmd)
		}
	}
}

func TestToolAllowedInModeMatrix(t *testing.T) {
	cases := []struct {
		mode    Mode
		tool    string
		allowed bool
	}{
		{ExploreMode, "read_file", true},
		{ExploreMode, "run_shell", true},
		{ExploreMode, "write_file", false},
		{ExploreMode, "edit_file", false},
		{ExploreMode, "switch_mode", true},
		{PlanMode, "read_file", true},
		{PlanMode, "run_shell", false},
		{PlanMode, "write_file", false},
		{PlanMode, "update_session_notes", true},
		{PlanMode, "switch_mode", true},
		{WriteMode, "run_shell", true},
		{WriteMode, "write_file", true},
		{WriteMode, "edit_file", true},
	}
	for _, c := range cases {
		m := &Model{mode: c.mode}
		got := m.toolAllowedInMode(c.tool)
		if got != c.allowed {
			t.Errorf("mode=%s tool=%s: expected %v, got %v", c.mode, c.tool, c.allowed, got)
		}
	}
}

func TestElapsedSuffix(t *testing.T) {
	m := &Model{}
	if got := m.elapsedSuffix(); got != "" {
		t.Errorf("idle: expected empty suffix, got %q", got)
	}
	m.busySince = time.Now().Add(-3 * time.Second)
	if got := m.elapsedSuffix(); got != " 3s" {
		t.Errorf("expected \" 3s\", got %q", got)
	}
	m.busySince = time.Now()
	if got := m.elapsedSuffix(); got != "" {
		t.Errorf("sub-second: expected empty suffix, got %q", got)
	}
}

func TestCurrentToolLabel(t *testing.T) {
	m := &Model{}
	if got := m.currentToolLabel(); got != "" {
		t.Errorf("no pending: expected empty, got %q", got)
	}
	m.pending = &pendingBatch{
		calls: []mcp.ToolCall{
			{Function: mcp.ToolCallFunction{Name: "read_file"}},
			{Function: mcp.ToolCallFunction{Name: "grep"}},
		},
		done: 1,
	}
	if got := m.currentToolLabel(); got != "grep" {
		t.Errorf("expected \"grep\" at done=1, got %q", got)
	}
	m.pending.done = 2
	if got := m.currentToolLabel(); got != "" {
		t.Errorf("all done: expected empty, got %q", got)
	}
}

func TestInputDynamicHeightGrowsOnWrappedText(t *testing.T) {
	input := textarea.New()
	input.Prompt = "› "
	input.ShowLineNumbers = false
	input.DynamicHeight = true
	input.MinHeight = minInputLines
	input.MaxHeight = maxInputLines
	input.SetWidth(12)
	input.SetHeight(minInputLines)
	input.Focus()
	input, _ = input.Update(nil)

	for _, r := range "abcdefghijklmnopqrstuvwxyz" {
		var cmd tea.Cmd
		input, cmd = input.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		if cmd != nil {
			_ = cmd()
		}
	}

	if input.Height() <= minInputLines {
		t.Fatalf("expected wrapped input to grow beyond %d line, got %d", minInputLines, input.Height())
	}
}

func TestAutoModePromptBypass(t *testing.T) {
	m := &Model{
		mode:  AutoMode,
		state: stateChat,
		pending: &pendingBatch{
			allowAll: false,
		},
	}

	// Test path inside workspace (trusted folder)
	callInside := mcp.ToolCall{
		Function: mcp.ToolCallFunction{
			Name:      "write_file",
			Arguments: json.RawMessage(`{"path":"src/main.go","content":"hello"}`),
		},
	}
	if m.shouldPromptPermission(callInside) {
		t.Error("expected shouldPromptPermission to be false for path inside trusted folder")
	}

	// Test path outside workspace (untrusted folder)
	callOutside := mcp.ToolCall{
		Function: mcp.ToolCallFunction{
			Name:      "write_file",
			Arguments: json.RawMessage(`{"path":"../../outside.txt","content":"hello"}`),
		},
	}
	if !m.shouldPromptPermission(callOutside) {
		t.Error("expected shouldPromptPermission to be true for path outside trusted folder")
	}
}

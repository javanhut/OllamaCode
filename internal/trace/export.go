package trace

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// DatasetRecord is one exported instruction-tuning example: the user turn plus
// the assistant's successful tool-call trajectory, as chat messages in the same
// envelope format the model saw at runtime. Tool results are verbatim redacted
// envelopes; export never un-redacts anything.
type DatasetRecord struct {
	Source   string        `json:"source"`           // trace file the record came from
	Model    string        `json:"model,omitempty"`  // model that produced the trajectory
	System   string        `json:"system,omitempty"` // system prompt, when the trace captured it
	Tools    []string      `json:"tools,omitempty"`  // tool names visible to the model
	Messages []api.Message `json:"messages"`         // user, assistant tool_calls, tool envelopes, final answer
}

// ExportOptions tunes which trajectories survive filtering.
type ExportOptions struct {
	MinCalls int // minimum successful tool calls per record; <= 0 means 1
}

// ExportStats summarizes filtering so drops are auditable.
type ExportStats struct {
	Candidates int            // trajectories found in the trace
	Kept       int            // records written
	Dropped    map[string]int // drop reason -> count
}

// Drop reasons reported in ExportStats.Dropped.
const (
	DropNoPrompt        = "no_prompt"        // user turn text not recoverable from the trace
	DropIncomplete      = "incomplete"       // turn never reached a completed turn_end (abandoned/limit/loop guard)
	DropToolError       = "tool_error"       // a tool call returned an error envelope
	DropArgumentFailure = "argument_failure" // a call needed argument repair, even a successful one
	DropTooFewCalls     = "too_few_calls"    // fewer successful calls than MinCalls
)

// Export reads one redacted trace and converts its successful tool-call
// trajectories into dataset records. Two trace shapes are recognized:
//
//   - Headless runs (agent.Run / cmd/eval) bracket a trajectory with
//     turn_start (task prompt, visible tools) and turn_end (outcome reason).
//     Only reason "completed" qualifies; "limit_or_loop_guard" is dropped.
//   - Interactive TUI turns carry no turn markers, so events are grouped by
//     their Turn number; the prompt and system context are recovered from the
//     first model_request payload of the turn. These traces record no explicit
//     completion signal, so health of the tool events is the only gate.
//
// A trajectory is kept only when every tool call succeeded on the first pass:
// any error result or any argument_failure (even one the repair loop recovered
// from) drops the whole trajectory, because the recorded assistant message may
// not match the repaired call that actually ran. Redacted content is passed
// through unchanged.
func Export(path string, opts ExportOptions) ([]DatasetRecord, ExportStats, error) {
	if opts.MinCalls <= 0 {
		opts.MinCalls = 1
	}
	stats := ExportStats{Dropped: map[string]int{}}
	var records []DatasetRecord
	finish := func(t *trajectory) {
		stats.Candidates++
		rec, reason := t.record(opts.MinCalls)
		if reason != "" {
			stats.Dropped[reason]++
			return
		}
		rec.Source = filepath.Base(path)
		records = append(records, rec)
	}

	var marked *trajectory
	groups := map[int]*trajectory{}
	var groupOrder []int
	// model_request payloads are deltas (see Recorder.RecordRequest), so the
	// system prompt and the current user turn usually appear only in the first
	// request that introduced them. Carry them forward as the replay advances.
	session := &trajectory{}
	err := Replay(path, func(ev Event) error {
		if ev.Kind == "model_request" {
			session.extractContext(ev)
		}
		switch {
		case ev.Kind == "turn_start":
			if marked != nil {
				finish(marked) // previous turn never closed: abandoned
			}
			marked = &trajectory{marked: true, model: ev.Model}
			if task, ok := ev.Metadata["task"].(string); ok {
				marked.prompt = task
			}
			marked.tools = stringSlice(ev.Metadata["tools"])
		case ev.Kind == "turn_end":
			if marked != nil {
				reason, _ := ev.Metadata["reason"].(string)
				marked.completed = reason == "completed"
				finish(marked)
				marked = nil
			}
		case ev.Turn > 0:
			// Interactive events are keyed by user-turn number; sub-agent and
			// eval events leave Turn unset and fall through to the marked turn.
			g := groups[ev.Turn]
			if g == nil {
				g = &trajectory{model: ev.Model}
				groups[ev.Turn] = g
				groupOrder = append(groupOrder, ev.Turn)
			}
			g.absorb(ev)
			g.inherit(session)
		default:
			if marked != nil {
				marked.absorb(ev)
				marked.inherit(session)
			}
		}
		return nil
	})
	if err != nil {
		return nil, stats, err
	}
	if marked != nil {
		finish(marked)
	}
	for _, turn := range groupOrder {
		finish(groups[turn])
	}
	stats.Kept = len(records)
	return records, stats, nil
}

// trajectory accumulates the events of one candidate training example.
type trajectory struct {
	model     string
	prompt    string
	system    string
	tools     []string
	seq       []Event // tool and payload-bearing model_response events, in order
	completed bool
	marked    bool // bounded by turn_start/turn_end (headless run)
}

func (t *trajectory) absorb(ev Event) {
	if t.model == "" {
		t.model = ev.Model
	}
	switch ev.Kind {
	case "tool":
		if ev.Tool != "" {
			t.seq = append(t.seq, ev)
		}
	case "model_response":
		if len(ev.Payload) > 0 {
			t.seq = append(t.seq, ev)
		}
	case "model_request":
		t.extractContext(ev)
	}
}

// inherit fills context the trajectory's own events never carried, from the
// session-wide view of the replay: with delta payloads a turn's request may
// contain nothing but tool results.
func (t *trajectory) inherit(session *trajectory) {
	if t.prompt == "" {
		t.prompt = session.prompt
	}
	if t.system == "" {
		t.system = session.system
	}
	if len(t.tools) == 0 {
		t.tools = session.tools
	}
}

// extractContext recovers the user prompt, system prompt, and visible tool
// names from a recorded model_request payload. The payload is the messages
// added since the previous request, so it may hold no user message at all; the
// last user message it does hold is the current turn's prompt.
func (t *trajectory) extractContext(ev Event) {
	if names := stringSlice(ev.Metadata["visible_tools"]); len(names) > 0 {
		t.tools = names
	}
	var msgs []api.Message
	if len(ev.Payload) == 0 || json.Unmarshal(ev.Payload, &msgs) != nil {
		return
	}
	// Only a payload that starts at message 0 can begin with the system prompt;
	// a delta starting mid-conversation begins with an injected mode banner.
	_, delta := ev.Metadata["payload_from"]
	if !delta && t.system == "" && len(msgs) > 0 && msgs[0].Role == "system" {
		t.system = msgs[0].Content
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			t.prompt = msgs[i].Content
			return
		}
	}
}

// record converts the trajectory into a dataset record, or reports the drop
// reason it failed a filter. Tool events between two model responses form one
// assistant tool-call round, preserving the call -> result -> next-call shape.
func (t *trajectory) record(minCalls int) (DatasetRecord, string) {
	rec := DatasetRecord{Model: t.model, System: t.system, Tools: t.tools}
	if strings.TrimSpace(t.prompt) == "" {
		return rec, DropNoPrompt
	}
	if t.marked && !t.completed {
		return rec, DropIncomplete
	}
	rec.Messages = append(rec.Messages, api.Message{Role: "user", Content: t.prompt})

	calls := 0
	finalAnswer := ""
	reason := ""
	var pending []Event // tool events of the current assistant round
	flush := func() {
		if len(pending) == 0 {
			return
		}
		call := api.Message{Role: "assistant"}
		for _, ev := range pending {
			call.ToolCalls = append(call.ToolCalls, tools.ToolCall{
				Function: tools.ToolCallFunction{Name: ev.Tool, Arguments: ev.Arguments},
			})
		}
		rec.Messages = append(rec.Messages, call)
		for _, ev := range pending {
			rec.Messages = append(rec.Messages, api.Message{Role: "tool", ToolName: ev.Tool, Content: ev.Result})
		}
		pending = nil
	}
	for _, ev := range t.seq {
		switch ev.Kind {
		case "tool":
			if ev.Error != "" {
				reason = DropToolError
			} else if failed, _ := ev.Metadata["argument_failure"].(bool); failed {
				reason = DropArgumentFailure
			}
			if reason != "" {
				break
			}
			calls++
			pending = append(pending, ev)
		case "model_response":
			flush()
			var resp api.ChatResponse
			if json.Unmarshal(ev.Payload, &resp) == nil && len(resp.Message.ToolCalls) == 0 {
				if content := strings.TrimSpace(resp.Message.Content); content != "" {
					finalAnswer = content
				}
			}
		}
		if reason != "" {
			break
		}
	}
	if reason != "" {
		return rec, reason
	}
	flush()
	if calls < minCalls {
		return rec, DropTooFewCalls
	}
	if finalAnswer != "" {
		rec.Messages = append(rec.Messages, api.Message{Role: "assistant", Content: finalAnswer})
	}
	return rec, ""
}

func stringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

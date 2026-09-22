package trace

import (
	"encoding/json"
	"path/filepath"
	"slices"
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
	MinCalls  int  // minimum successful tool calls per record; <= 0 means 1
	OnlyRated bool // keep only turns a human rated good with /rate
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
	DropRatedBad        = "rated_bad"        // a human rated the turn bad with /rate
	DropUnrated         = "unrated"          // OnlyRated is set and no rating covers the turn
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
//
// Human ratings ride the same trace as turn_rating events (tui /rate). A turn
// rated bad is dropped unconditionally: "it completed" is exactly the signal
// that cannot tell a right answer from a wrong one, which is why the rating
// exists.
func Export(path string, opts ExportOptions) ([]DatasetRecord, ExportStats, error) {
	if opts.MinCalls <= 0 {
		opts.MinCalls = 1
	}
	stats := ExportStats{Dropped: map[string]int{}}
	var records []DatasetRecord
	finish := func(t *trajectory) {
		stats.Candidates++
		rec, reason := t.record(opts)
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
	var spans []ratingSpan
	// model_request payloads are deltas (see Recorder.RecordRequest), so the
	// system prompt and the current user turn usually appear only in the first
	// request that introduced them. Carry them forward as the replay advances.
	session := &trajectory{}
	// endSession closes out everything one recorded session accumulated. It is
	// what keeps a turn number meaningful: generations restart at 1 in every
	// process, and the configured trace is opened O_APPEND, so without a
	// boundary groups[1] would splice one run's turn 1 onto an unrelated run's
	// turn 1 — and a /rate verdict typed in the second run would decide the
	// export fate of the first run's identically-numbered turns.
	endSession := func() {
		for _, turn := range groupOrder {
			g := groups[turn]
			g.rating = ratingFor(spans, turn)
			finish(g)
		}
		groups, groupOrder, spans = map[int]*trajectory{}, nil, nil
		session = &trajectory{} // its prompt and system prompt belong to the run that just ended
	}
	err := Replay(path, func(ev Event) error {
		if ev.Kind == "model_request" {
			session.extractContext(ev)
		}
		switch {
		case ev.Kind == "session_start":
			if marked != nil {
				finish(marked) // a headless run that never closed its last turn
				marked = nil
			}
			endSession()
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
		case ev.Kind == "turn_rating":
			// Recorded when the user types /rate, long after the turn's own
			// events and deliberately with no Event.Turn: matched above the
			// Turn > 0 case so a rating can never open a group of its own.
			if span, ok := ratingSpanOf(ev); ok {
				spans = append(spans, span)
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
	// Headless turns (finish(marked) above) are never rated: /rate exists only
	// in the TUI, and those trajectories close mid-replay anyway, before a
	// later rating event could be read.
	endSession()
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
	marked    bool   // bounded by turn_start/turn_end (headless run)
	rating    string // human verdict from /rate; "" when unrated
}

// ratingSpan is one /rate verdict and the generations it covers. A range, not
// a single number, because the TUI bumps its generation once per tool round:
// one user turn is a span of generations, and the last one holds only the
// final prose reply — a rating naming that generation alone would land on the
// one group the exporter already discards for having no tool calls.
type ratingSpan struct {
	from, to int
	rating   string
}

// ratingFor returns the verdict covering turn, scanning backwards so a user
// who changed their mind gets the last word. A slice plus this scan costs one
// pass per group; expanding spans into a map would let a corrupt trace with a
// huge range allocate without bound.
func ratingFor(spans []ratingSpan, turn int) string {
	for _, span := range slices.Backward(spans) {
		if turn >= span.from && turn <= span.to {
			return span.rating
		}
	}
	return ""
}

func ratingSpanOf(ev Event) (ratingSpan, bool) {
	rating, _ := ev.Metadata["rating"].(string)
	if rating != "good" && rating != "bad" {
		return ratingSpan{}, false
	}
	to := metaInt(ev.Metadata["turn"])
	if to <= 0 {
		return ratingSpan{}, false
	}
	from := metaInt(ev.Metadata["from_turn"])
	if from <= 0 || from > to {
		from = to // older or malformed ratings name a single generation
	}
	return ratingSpan{from: from, to: to, rating: rating}, true
}

// metaInt reads a metadata number: Replay decodes JSON numbers as float64.
func metaInt(value any) int {
	if f, ok := value.(float64); ok {
		return int(f)
	}
	n, _ := value.(int)
	return n
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
	for _, msg := range slices.Backward(msgs) {
		// Advisory skipped: a recorded request carries the loop guards'
		// user-role nudges too, and "[REPEATING ACTION] …" is not the turn's
		// prompt for a training record.
		if msg.Role == "user" && !msg.Advisory {
			t.prompt = msg.Content
			return
		}
	}
}

// record converts the trajectory into a dataset record, or reports the drop
// reason it failed a filter. Tool events between two model responses form one
// assistant tool-call round, preserving the call -> result -> next-call shape.
func (t *trajectory) record(opts ExportOptions) (DatasetRecord, string) {
	rec := DatasetRecord{Model: t.model, System: t.system, Tools: t.tools}
	// The human verdict outranks every mechanical filter, so it is checked
	// first: a trajectory a human called wrong must never be reported as
	// dropped for some incidental reason that would go away on the next run.
	if t.rating == "bad" {
		return rec, DropRatedBad
	}
	if opts.OnlyRated && t.rating != "good" {
		return rec, DropUnrated
	}
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
	if calls < opts.MinCalls {
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

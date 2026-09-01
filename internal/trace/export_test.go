package trace

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/javanhut/ollama_code/api"
)

// writeTrace records synthetic events through the real Recorder so tests
// exercise the same redaction and encoding path as production traces.
func writeTrace(t *testing.T, events ...Event) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if err := r.Record(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func toolEvent(name, args, result string) Event {
	return Event{Kind: "tool", Tool: name, Arguments: json.RawMessage(args), Result: result,
		Metadata: map[string]any{"argument_failure": false, "repair_attempted": false, "repair_succeeded": false}}
}

func ratingEvent(from, to int, rating string) Event {
	return Event{Kind: "turn_rating", Metadata: map[string]any{"from_turn": from, "turn": to, "rating": rating}}
}

func proseResponse(t *testing.T, content string) Event {
	t.Helper()
	payload, err := json.Marshal(api.ChatResponse{Message: api.Message{Role: "assistant", Content: content}})
	if err != nil {
		t.Fatal(err)
	}
	return Event{Kind: "model_response", Payload: payload}
}

func TestExportFiltersAndFormats(t *testing.T) {
	okEnvelope := `{"ok":true,"summary":"read_file completed","evidence":["package x"]}`
	tests := []struct {
		name       string
		events     []Event
		minCalls   int
		wantKept   int
		wantReason string // expected single drop reason when wantKept == 0
	}{
		{
			name: "completed headless run is kept",
			events: []Event{
				{Kind: "turn_start", Metadata: map[string]any{"task": "fix the bug", "tools": []any{"read_file", "edit_file"}}},
				toolEvent("read_file", `{"path":"x.go"}`, okEnvelope),
				proseResponse(t, "done"),
				{Kind: "turn_end", Metadata: map[string]any{"reason": "completed"}},
			},
			wantKept: 1,
		},
		{
			name: "limit or loop guard end is dropped",
			events: []Event{
				{Kind: "turn_start", Metadata: map[string]any{"task": "fix the bug"}},
				toolEvent("read_file", `{"path":"x.go"}`, okEnvelope),
				{Kind: "turn_end", Metadata: map[string]any{"reason": "limit_or_loop_guard"}},
			},
			wantReason: DropIncomplete,
		},
		{
			name: "missing turn_end means abandoned",
			events: []Event{
				{Kind: "turn_start", Metadata: map[string]any{"task": "fix the bug"}},
				toolEvent("read_file", `{"path":"x.go"}`, okEnvelope),
			},
			wantReason: DropIncomplete,
		},
		{
			name: "tool error drops the whole trajectory",
			events: []Event{
				{Kind: "turn_start", Metadata: map[string]any{"task": "fix the bug"}},
				toolEvent("read_file", `{"path":"x.go"}`, okEnvelope),
				{Kind: "tool", Tool: "edit_file", Arguments: json.RawMessage(`{"path":"x.go"}`), Error: "old_string not found",
					Metadata: map[string]any{"argument_failure": false}},
				{Kind: "turn_end", Metadata: map[string]any{"reason": "completed"}},
			},
			wantReason: DropToolError,
		},
		{
			name: "repaired argument failure still drops",
			events: []Event{
				{Kind: "turn_start", Metadata: map[string]any{"task": "fix the bug"}},
				{Kind: "tool", Tool: "read_file", Arguments: json.RawMessage(`{"path":"x.go"}`), Result: okEnvelope,
					Metadata: map[string]any{"argument_failure": true, "repair_attempted": true, "repair_succeeded": true}},
				{Kind: "turn_end", Metadata: map[string]any{"reason": "completed"}},
			},
			wantReason: DropArgumentFailure,
		},
		{
			name: "turn without a task prompt is dropped",
			events: []Event{
				{Kind: "turn_start", Metadata: map[string]any{}},
				toolEvent("read_file", `{"path":"x.go"}`, okEnvelope),
				{Kind: "turn_end", Metadata: map[string]any{"reason": "completed"}},
			},
			wantReason: DropNoPrompt,
		},
		{
			name: "min-calls filters short trajectories",
			events: []Event{
				{Kind: "turn_start", Metadata: map[string]any{"task": "fix the bug"}},
				toolEvent("read_file", `{"path":"x.go"}`, okEnvelope),
				{Kind: "turn_end", Metadata: map[string]any{"reason": "completed"}},
			},
			minCalls:   2,
			wantReason: DropTooFewCalls,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTrace(t, tc.events...)
			records, stats, err := Export(path, ExportOptions{MinCalls: tc.minCalls})
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != tc.wantKept {
				t.Fatalf("kept %d records, want %d (dropped %v)", len(records), tc.wantKept, stats.Dropped)
			}
			if tc.wantReason != "" && stats.Dropped[tc.wantReason] != 1 {
				t.Fatalf("expected one %s drop, got %v", tc.wantReason, stats.Dropped)
			}
		})
	}
}

func TestExportRecordShape(t *testing.T) {
	envelope := `{"ok":true,"summary":"read_file completed"}`
	path := writeTrace(t,
		Event{Kind: "turn_start", Model: "qwen2.5-coder:7b", Metadata: map[string]any{"task": "read x.go", "tools": []any{"read_file"}}},
		toolEvent("read_file", `{"path":"x.go"}`, envelope),
		proseResponse(t, "x.go declares package x"),
		Event{Kind: "turn_end", Metadata: map[string]any{"reason": "completed"}},
	)
	records, _, err := Export(path, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("kept %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.Source != "trace.jsonl" || rec.Model != "qwen2.5-coder:7b" {
		t.Fatalf("unexpected record metadata: %#v", rec)
	}
	if len(rec.Tools) != 1 || rec.Tools[0] != "read_file" {
		t.Fatalf("unexpected tools: %#v", rec.Tools)
	}
	wantRoles := []string{"user", "assistant", "tool", "assistant"}
	if len(rec.Messages) != len(wantRoles) {
		t.Fatalf("got %d messages, want %d: %#v", len(rec.Messages), len(wantRoles), rec.Messages)
	}
	for i, role := range wantRoles {
		if rec.Messages[i].Role != role {
			t.Fatalf("message %d role %q, want %q", i, rec.Messages[i].Role, role)
		}
	}
	if rec.Messages[0].Content != "read x.go" {
		t.Fatalf("unexpected prompt: %q", rec.Messages[0].Content)
	}
	call := rec.Messages[1]
	if len(call.ToolCalls) != 1 || call.ToolCalls[0].Function.Name != "read_file" || string(call.ToolCalls[0].Function.Arguments) != `{"path":"x.go"}` {
		t.Fatalf("unexpected tool call: %#v", call.ToolCalls)
	}
	if rec.Messages[2].ToolName != "read_file" || rec.Messages[2].Content != envelope {
		t.Fatalf("unexpected tool result: %#v", rec.Messages[2])
	}
	if rec.Messages[3].Content != "x.go declares package x" {
		t.Fatalf("unexpected final answer: %q", rec.Messages[3].Content)
	}
}

func TestExportGroupsInteractiveTurns(t *testing.T) {
	msgs, _ := json.Marshal([]api.Message{
		{Role: "system", Content: "you are ocode"},
		{Role: "user", Content: "count the go files"},
	})
	envelope := `{"ok":true,"summary":"list_directory completed"}`
	// Interactive traces carry no turn markers; events group by Turn number and
	// the prompt comes from the recorded model_request payload.
	path := writeTrace(t,
		Event{Kind: "model_request", Turn: 1, Payload: msgs, Metadata: map[string]any{"visible_tools": []any{"list_directory"}}},
		Event{Kind: "tool", Turn: 1, Tool: "list_directory", Arguments: json.RawMessage(`{"path":"."}`), Result: envelope,
			Metadata: map[string]any{"argument_failure": false}},
	)
	records, _, err := Export(path, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("kept %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.System != "you are ocode" || len(rec.Tools) != 1 || rec.Tools[0] != "list_directory" {
		t.Fatalf("unexpected context: %#v", rec)
	}
	if rec.Messages[0].Role != "user" || rec.Messages[0].Content != "count the go files" {
		t.Fatalf("unexpected prompt message: %#v", rec.Messages[0])
	}
	if len(rec.Messages) != 3 { // no final answer was recorded; trajectory ends at the tool result
		t.Fatalf("got %d messages, want 3", len(rec.Messages))
	}
}

func TestExportPreservesRoundBoundaries(t *testing.T) {
	envelope := `{"ok":true,"summary":"ok"}`
	// Two tool rounds separated by a model response become two assistant
	// tool-call messages, not one merged batch.
	path := writeTrace(t,
		Event{Kind: "turn_start", Metadata: map[string]any{"task": "two steps"}},
		toolEvent("read_file", `{"path":"a.go"}`, envelope),
		proseResponse(t, ""), // payload-bearing boundary; empty content is not a final answer
		toolEvent("read_file", `{"path":"b.go"}`, envelope),
		proseResponse(t, "both read"),
		Event{Kind: "turn_end", Metadata: map[string]any{"reason": "completed"}},
	)
	records, _, err := Export(path, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("kept %d records, want 1", len(records))
	}
	var assistantCalls, toolResults int
	for _, msg := range records[0].Messages {
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			assistantCalls++
		}
		if msg.Role == "tool" {
			toolResults++
		}
	}
	if assistantCalls != 2 || toolResults != 2 {
		t.Fatalf("got %d assistant rounds and %d results, want 2 and 2: %#v", assistantCalls, toolResults, records[0].Messages)
	}
}

func TestExportRecoversContextFromDeltaPayloads(t *testing.T) {
	first, _ := json.Marshal([]api.Message{
		{Role: "system", Content: "you are ocode"},
		{Role: "user", Content: "count the go files"},
	})
	// A later turn's request payload holds only what was appended since the
	// previous one — no user message, and a mode banner where the system
	// prompt used to sit.
	delta, _ := json.Marshal([]api.Message{
		{Role: "tool", Content: `{"ok":true}`},
		{Role: "system", Content: "Current mode: explore"},
	})
	envelope := `{"ok":true,"summary":"list_directory completed"}`
	path := writeTrace(t,
		Event{Kind: "model_request", Turn: 1, Payload: first, Metadata: map[string]any{"visible_tools": []any{"list_directory"}}},
		Event{Kind: "tool", Turn: 1, Tool: "list_directory", Arguments: json.RawMessage(`{"path":"."}`), Result: envelope},
		Event{Kind: "model_request", Turn: 2, Payload: delta, Metadata: map[string]any{"payload_from": 2, "visible_tools": []any{"grep"}}},
		Event{Kind: "tool", Turn: 2, Tool: "grep", Arguments: json.RawMessage(`{"pattern":"func"}`), Result: envelope},
	)
	records, stats, err := Export(path, ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("kept %d records, want 2 (dropped: %v)", len(records), stats.Dropped)
	}
	for i, rec := range records {
		if rec.System != "you are ocode" {
			t.Fatalf("record %d system = %q", i, rec.System)
		}
		if rec.Messages[0].Content != "count the go files" {
			t.Fatalf("record %d prompt = %q", i, rec.Messages[0].Content)
		}
	}
}

// TestExportRatingFilter covers the /rate verdicts recorded as turn_rating
// events. Interactive event shape throughout: ratings only exist for TUI
// turns, which are the ones grouped by Turn number.
func TestExportRatingFilter(t *testing.T) {
	envelope := `{"ok":true,"summary":"list_directory completed"}`
	request, _ := json.Marshal([]api.Message{
		{Role: "system", Content: "you are ocode"},
		{Role: "user", Content: "count the go files"},
	})
	// One clean interactive round of the generation turn asks for.
	round := func(turn int) []Event {
		return []Event{
			{Kind: "model_request", Turn: turn, Payload: request, Metadata: map[string]any{"visible_tools": []any{"list_directory"}}},
			{Kind: "tool", Turn: turn, Tool: "list_directory", Arguments: json.RawMessage(`{"path":"."}`), Result: envelope,
				Metadata: map[string]any{"argument_failure": false}},
		}
	}
	export := func(t *testing.T, opts ExportOptions, events ...Event) ([]DatasetRecord, ExportStats) {
		t.Helper()
		records, stats, err := Export(writeTrace(t, events...), opts)
		if err != nil {
			t.Fatal(err)
		}
		return records, stats
	}

	t.Run("rated bad drops a clean trajectory", func(t *testing.T) {
		events := append(round(1), ratingEvent(1, 1, "bad"))
		records, stats := export(t, ExportOptions{}, events...)
		if len(records) != 0 || stats.Dropped[DropRatedBad] != 1 {
			t.Fatalf("kept %d records, dropped %v; want 0 kept and one %s", len(records), stats.Dropped, DropRatedBad)
		}
	})

	t.Run("unrated kept by default dropped under only-rated", func(t *testing.T) {
		records, _ := export(t, ExportOptions{}, round(1)...)
		if len(records) != 1 {
			t.Fatalf("kept %d records by default, want 1", len(records))
		}
		records, stats := export(t, ExportOptions{OnlyRated: true}, round(1)...)
		if len(records) != 0 || stats.Dropped[DropUnrated] != 1 {
			t.Fatalf("kept %d records under OnlyRated, dropped %v; want 0 kept and one %s", len(records), stats.Dropped, DropUnrated)
		}
	})

	t.Run("last rating wins", func(t *testing.T) {
		events := append(round(1), ratingEvent(1, 1, "bad"), ratingEvent(1, 1, "good"))
		records, stats := export(t, ExportOptions{OnlyRated: true}, events...)
		if len(records) != 1 || stats.Dropped[DropRatedBad] != 0 {
			t.Fatalf("bad-then-good kept %d records, dropped %v; want 1 kept and no %s", len(records), stats.Dropped, DropRatedBad)
		}
		events = append(round(1), ratingEvent(1, 1, "good"), ratingEvent(1, 1, "bad"))
		records, stats = export(t, ExportOptions{}, events...)
		if len(records) != 0 || stats.Dropped[DropRatedBad] != 1 {
			t.Fatalf("good-then-bad kept %d records, dropped %v; want 0 kept and one %s", len(records), stats.Dropped, DropRatedBad)
		}
	})

	t.Run("rating for a turn with no record", func(t *testing.T) {
		records, stats := export(t, ExportOptions{},
			Event{Kind: "model_request", Payload: request},
			ratingEvent(7, 7, "good"))
		if len(records) != 0 || stats.Candidates != 0 {
			t.Fatalf("got %d records and %d candidates, want 0 and 0: a rating must not open a trajectory", len(records), stats.Candidates)
		}
	})

	t.Run("rating spans every round of a turn", func(t *testing.T) {
		// Two tool rounds of one user turn: startStream bumps the generation
		// per round, so a rating naming only the last one would leave the
		// first round in the dataset.
		events := append(round(1), round(2)...)
		events = append(events, ratingEvent(1, 2, "bad"))
		records, stats := export(t, ExportOptions{}, events...)
		if len(records) != 0 || stats.Dropped[DropRatedBad] != 2 {
			t.Fatalf("kept %d records, dropped %v; want 0 kept and two %s", len(records), stats.Dropped, DropRatedBad)
		}
	})

	// The configured trace is opened O_APPEND and generations restart at 1 in
	// every process, so a rating typed today must not decide the fate of
	// yesterday's identically-numbered turn — nor may the two turns' events be
	// concatenated into one record.
	t.Run("ratings and groups are scoped to their session", func(t *testing.T) {
		events := append(round(1), Event{Kind: "session_start"})
		events = append(events, round(1)...)
		events = append(events, ratingEvent(1, 1, "bad"))
		records, stats := export(t, ExportOptions{}, events...)
		if len(records) != 1 || stats.Candidates != 2 || stats.Dropped[DropRatedBad] != 1 {
			t.Fatalf("kept %d of %d candidates, dropped %v; want the rated session dropped and the earlier one kept",
				len(records), stats.Candidates, stats.Dropped)
		}
		if n := len(records[0].Messages); n != 3 {
			t.Fatalf("the kept record has %d messages, want 3: the two sessions' turn 1 were spliced together", n)
		}
		records, stats = export(t, ExportOptions{OnlyRated: true}, events...)
		if len(records) != 0 || stats.Dropped[DropUnrated] != 1 {
			t.Fatalf("kept %d records under OnlyRated, dropped %v; want the unrated earlier session dropped", len(records), stats.Dropped)
		}
	})

	t.Run("malformed rating is ignored", func(t *testing.T) {
		events := append(round(1), ratingEvent(1, 1, "maybe"), ratingEvent(0, 0, "bad"))
		records, stats := export(t, ExportOptions{}, events...)
		if len(records) != 1 || stats.Dropped[DropRatedBad] != 0 {
			t.Fatalf("kept %d records, dropped %v; want the turn kept as unrated", len(records), stats.Dropped)
		}
	})
}

package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// fakeChat returns scripted responses in order, recording the requests it saw.
type fakeChat struct {
	responses []api.ChatResponse
	calls     int
}

func (f *fakeChat) ChatOnce(_ context.Context, _ api.ChatRequest) (api.ChatResponse, error) {
	r := f.responses[f.calls]
	f.calls++
	return r, nil
}

func echoRegistry(seen *string) *tools.Registry {
	r := tools.NewRegistry()
	r.Register(tools.Tool{
		Function: tools.Function{
			Name:       "echo",
			Parameters: tools.Schema{Type: "object", Properties: map[string]tools.Property{"text": {Type: "string"}}},
		},
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(args, &a)
			*seen = a.Text
			return "echoed: " + a.Text, nil
		},
	})
	return r
}

func toolResp(name, args string) api.ChatResponse {
	return api.ChatResponse{Message: api.Message{
		ToolCalls: []tools.ToolCall{{Function: tools.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}},
	}}
}

func textResp(s string) api.ChatResponse {
	return api.ChatResponse{Message: api.Message{Content: s}}
}

func TestRun_DispatchesToolThenAnswers(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"hi"}`),
		textResp("all done"),
	}}
	res, err := Run(context.Background(), host, reg, "do it", Options{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if seen != "hi" {
		t.Fatalf("tool not dispatched with args, seen=%q", seen)
	}
	if res.Output != "all done" {
		t.Fatalf("final output = %q", res.Output)
	}
	if res.Steps != 1 {
		t.Fatalf("expected 1 tool step, got %d", res.Steps)
	}
}

func TestRun_ToolFilterBlocks(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"hi"}`),
		textResp("done"),
	}}
	// Filter permits nothing -> the echo call must be refused, not executed.
	res, err := Run(context.Background(), host, reg, "do it", Options{
		Model:      "m",
		ToolFilter: func(string) bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != "" {
		t.Fatalf("filtered tool should not have run, seen=%q", seen)
	}
	if res.Output != "done" {
		t.Fatalf("output=%q", res.Output)
	}
}

func TestRun_EscalatesBadArgsViaFormat(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	// Round 1: model emits invalid-JSON args -> Invoke fails validation ->
	// escalation asks for a fix (2nd response, schema-constrained) -> retry
	// succeeds. Round 2: model answers.
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text": bad}`), // malformed
		textResp(`{"text":"fixed"}`),      // format-repair reply
		textResp("done"),                  // final answer
	}}
	res, err := Run(context.Background(), host, reg, "do it", Options{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if seen != "fixed" {
		t.Fatalf("escalation should have repaired args and dispatched; seen=%q", seen)
	}
	if res.Output != "done" {
		t.Fatalf("output=%q", res.Output)
	}
	if host.calls != 3 {
		t.Fatalf("expected 3 chat calls (call, repair, answer), got %d", host.calls)
	}
	if res.ArgumentFailures != 1 || res.RepairAttempts != 1 || res.RepairsSucceeded != 1 {
		t.Fatalf("unexpected repair metrics: %+v", res)
	}
}

func TestRun_StuckGuardBreaksAndFinalizes(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	// The model spams the identical call. Budget is 8, but the stuck-guard should
	// dispatch it only maxIdenticalCalls (2) times, refuse the 3rd, see no
	// progress, and break to a tool-less finalize — returning useful output well
	// before the step cap instead of the old "hit the limit" sentinel.
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"same"}`), // dispatched (count 1)
		toolResp("echo", `{"text":"same"}`), // dispatched (count 2)
		toolResp("echo", `{"text":"same"}`), // refused -> no progress -> break
		textResp("final report"),            // finalize (no tools available)
	}}
	res, err := Run(context.Background(), host, reg, "task", Options{Model: "m", MaxSteps: 8})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "final report" {
		t.Fatalf("expected finalized report, got %q", res.Output)
	}
	if !res.HitLimit {
		t.Fatal("expected HitLimit=true (broke via guard, didn't answer naturally)")
	}
	if res.Steps != 3 {
		t.Fatalf("guard should break at step 3, not run to the cap; got %d", res.Steps)
	}
	if host.calls != 4 {
		t.Fatalf("expected 4 chat calls (3 rounds + finalize), got %d", host.calls)
	}
}

func TestRun_StepLimit(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	// Always returns a tool call -> should hit the step cap.
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"a"}`),
		toolResp("echo", `{"text":"b"}`),
		toolResp("echo", `{"text":"c"}`),
	}}
	res, err := Run(context.Background(), host, reg, "loop", Options{Model: "m", MaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !res.HitLimit {
		t.Fatal("expected HitLimit=true")
	}
	if res.Steps != 2 {
		t.Fatalf("expected 2 steps, got %d", res.Steps)
	}
}

// recordChat is a fakeChat that also captures the requests it saw.
type recordChat struct {
	responses []api.ChatResponse
	calls     int
	requests  []api.ChatRequest
}

func (f *recordChat) ChatOnce(_ context.Context, req api.ChatRequest) (api.ChatResponse, error) {
	f.requests = append(f.requests, req)
	r := f.responses[f.calls]
	f.calls++
	return r, nil
}

// TestRun_SeedsPriorMessagesAndReturnsHistory covers the follow-up contract:
// a run seeded with a finished child's conversation sends that history plus
// the follow-up to the model, gets a FRESH step budget (the seed's rounds
// don't count), and returns the full conversation for re-retention.
func TestRun_SeedsPriorMessagesAndReturnsHistory(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	prior := []api.Message{
		{Role: "system", Content: "sub-agent system"},
		{Role: "user", Content: "original task"},
		{Role: "assistant", Content: "original report"},
	}
	host := &recordChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"more"}`),
		textResp("follow-up report"),
	}}
	res, err := Run(context.Background(), host, reg, "what about y?", Options{
		Model:         "m",
		System:        "ignored — seed leads with its own system",
		MaxSteps:      5,
		PriorMessages: prior,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The first request must carry the seed verbatim plus the follow-up.
	got := host.requests[0].Messages
	if len(got) != len(prior)+1 {
		t.Fatalf("expected seed + follow-up (%d messages), got %d: %#v", len(prior)+1, len(got), got)
	}
	for i, m := range prior {
		if !reflect.DeepEqual(got[i], m) {
			t.Fatalf("seed message %d changed: want %#v, got %#v", i, m, got[i])
		}
	}
	if got[len(prior)].Role != "user" || got[len(prior)].Content != "what about y?" {
		t.Fatalf("follow-up not appended as the next user message: %#v", got[len(prior)])
	}
	// Fresh budget: this run used 1 step; the seed's rounds don't count.
	if res.Steps != 1 {
		t.Fatalf("expected a fresh step budget (1 step), got %d", res.Steps)
	}
	// The returned history is the full conversation: seed, follow-up, the
	// tool round, and the final answer — ready to seed the next follow-up.
	want := len(prior) + 1 + 2 + 1 // seed + follow-up + (assistant call + tool result) + final answer
	if len(res.Messages) != want {
		t.Fatalf("expected %d retained messages, got %d: %#v", want, len(res.Messages), res.Messages)
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != "assistant" || last.Content != "follow-up report" {
		t.Fatalf("retained history must end with the final answer, got %#v", last)
	}
}

// TestRun_HitLimitHistoryIncludesFinalize: the retained history of a
// limit-hitting run keeps the finalize exchange too, so a follow-up sees what
// the child concluded.
func TestRun_HitLimitHistoryIncludesFinalize(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	host := &recordChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"a"}`),
		textResp("partial report"),
	}}
	res, err := Run(context.Background(), host, reg, "loop", Options{Model: "m", MaxSteps: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !res.HitLimit {
		t.Fatal("expected HitLimit=true")
	}
	last := res.Messages[len(res.Messages)-1]
	if last.Role != "assistant" || last.Content != "partial report" {
		t.Fatalf("retained history must end with the finalized report, got %#v", last)
	}
	advisory := res.Messages[len(res.Messages)-2]
	if advisory.Role != "user" || !advisory.Advisory {
		t.Fatalf("expected the advisory finalize nudge before the report, got %#v", advisory)
	}
}

// TestRun_ErrorRetainsPartialHistory: a failed run still returns the
// exchanges up to the failure, so an interrupted child can be resumed.
func TestRun_ErrorRetainsPartialHistory(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	host := &recordChat{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"a"}`),
	}}
	fail := &failingChat{inner: host, failOn: 1}
	res, err := Run(context.Background(), fail, reg, "task", Options{Model: "m"})
	if err == nil {
		t.Fatal("expected the host error to surface")
	}
	// system? none. user task + assistant call + tool result = 3.
	if len(res.Messages) != 3 {
		t.Fatalf("expected the partial history up to the failure, got %#v", res.Messages)
	}
}

// failingChat wraps recordChat and errors on one call, like a dropped
// connection or a cancelled context mid-run.
type failingChat struct {
	inner  *recordChat
	failOn int
}

func (f *failingChat) ChatOnce(ctx context.Context, req api.ChatRequest) (api.ChatResponse, error) {
	if f.inner.calls == f.failOn {
		f.inner.calls++
		return api.ChatResponse{}, context.DeadlineExceeded
	}
	return f.inner.ChatOnce(ctx, req)
}

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

// fakeOllama is a scripted ChatClient that presents as a native Ollama
// endpoint (the constrained-decoding gate keys off IsOpenAI/IsCursor) and
// records every request it saw. reject simulates a host refusing a format
// payload; requests are recorded before rejection so tests can inspect what
// rung was attempted.
type fakeOllama struct {
	responses []api.ChatResponse
	requests  []api.ChatRequest
	reject    func(format json.RawMessage) error
}

func (f *fakeOllama) ChatOnce(_ context.Context, req api.ChatRequest) (api.ChatResponse, error) {
	f.requests = append(f.requests, req)
	if f.reject != nil {
		if err := f.reject(req.Format); err != nil {
			return api.ChatResponse{}, err
		}
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	return r, nil
}

func (f *fakeOllama) IsOpenAI() bool { return false }
func (f *fakeOllama) IsCursor() bool { return false }

func schemaRejection() error {
	// Mirrors api.statusError for Ollama's schema->grammar conversion failure.
	return fmt.Errorf(`unexpected status code: 400: {"error": "JSON schema conversion failed: Unrecognized schema"}`)
}

func formatToolDefs(t *testing.T) []tools.Tool {
	t.Helper()
	r := tools.NewRegistry()
	r.Register(tools.Tool{Function: tools.Function{
		Name: "read_file",
		Parameters: tools.Schema{Type: "object",
			Properties: map[string]tools.Property{"path": {Type: "string"}},
			Required:   []string{"path"}},
	}})
	r.Register(tools.Tool{Function: tools.Function{
		Name: "grep",
		Parameters: tools.Schema{Type: "object",
			Properties: map[string]tools.Property{"pattern": {Type: "string"}},
			Required:   []string{"pattern"}},
	}})
	return r.Definitions()
}

func TestToolCallFormat_FullUnion(t *testing.T) {
	defs := formatToolDefs(t)
	raw := ToolCallFormat(RungFullUnion, defs)
	var schema struct {
		AnyOf []map[string]json.RawMessage `json:"anyOf"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	// One branch per tool plus the prose escape branch.
	if len(schema.AnyOf) != len(defs)+1 {
		t.Fatalf("expected %d branches (tools + prose), got %d", len(defs)+1, len(schema.AnyOf))
	}
	s := string(raw)
	for _, name := range []string{"read_file", "grep"} {
		if !strings.Contains(s, `"`+name+`"`) {
			t.Fatalf("union is missing a branch for %q: %s", name, s)
		}
	}
	// Per-tool argument constraints ride along (path required on read_file).
	if !strings.Contains(s, `"path"`) || !strings.Contains(s, `"pattern"`) {
		t.Fatalf("union lost per-tool argument schemas: %s", s)
	}
	if !strings.Contains(s, `"response"`) {
		t.Fatalf("union lost the prose escape branch: %s", s)
	}
}

func TestToolCallFormat_NameEnum(t *testing.T) {
	defs := formatToolDefs(t)
	raw := ToolCallFormat(RungNameEnum, defs)
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if _, hasUnion := schema["anyOf"]; hasUnion {
		t.Fatal("flat rung must not use anyOf — unions are what it falls back from")
	}
	s := string(raw)
	for _, want := range []string{`"enum":["grep","read_file"]`, `"response"`, `"arguments"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("flat schema missing %s: %s", want, s)
		}
	}
	// No required fields: forcing name+arguments would outlaw the prose branch.
	if _, hasRequired := schema["required"]; hasRequired {
		t.Fatal("flat rung must not require name/arguments — that would force a tool call")
	}
}

func TestToolCallFormat_PlainJSON(t *testing.T) {
	if got := string(ToolCallFormat(RungPlainJSON, nil)); got != `"json"` {
		t.Fatalf("plain-JSON rung = %s, want \"json\"", got)
	}
	if got := ToolCallFormat(RungOff, nil); got != nil {
		t.Fatalf("off rung should produce no format, got %s", got)
	}
}

func TestConstraintCache_WalksTheLadderOnce(t *testing.T) {
	defs := formatToolDefs(t)
	c := NewConstraintCache()
	key := "host\x00model"

	has := func(raw json.RawMessage, sub string) bool { return strings.Contains(string(raw), sub) }

	raw, ok := c.Format(key, defs)
	if !ok || !has(raw, "anyOf") {
		t.Fatalf("first probe should be the full union, got ok=%v format=%s", ok, raw)
	}
	if !c.Downgrade(key) {
		t.Fatal("downgrade from full union should succeed")
	}
	raw, ok = c.Format(key, defs)
	if !ok || has(raw, "anyOf") || !has(raw, `"enum"`) {
		t.Fatalf("second rung should be the flat enum schema, got ok=%v format=%s", ok, raw)
	}
	if !c.Downgrade(key) {
		t.Fatal("downgrade from flat enum should succeed")
	}
	raw, ok = c.Format(key, defs)
	if !ok || string(raw) != `"json"` {
		t.Fatalf("third rung should be plain JSON, got ok=%v format=%s", ok, raw)
	}
	if !c.Downgrade(key) {
		t.Fatal("downgrade from plain JSON should succeed (to unconstrained)")
	}
	if _, ok = c.Format(key, defs); ok {
		t.Fatal("after full rejection the pair must stay unconstrained")
	}
	if c.Downgrade(key) {
		t.Fatal("downgrade at the bottom rung should report failure")
	}
	// A different model+host probes independently.
	if raw, ok := c.Format("host\x00other", defs); !ok || !has(raw, "anyOf") {
		t.Fatal("cache must be keyed per model+host, not process-wide")
	}
	// Nothing to constrain to -> inactive even before any probing.
	if _, ok := c.Format("host\x00fresh", nil); ok {
		t.Fatal("empty tool list is a prose turn — no format")
	}
}

func TestUnwrapConstrainedProse(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{`{"response": "all done"}`, "all done", true},
		{"{\n  \"response\": \"2 + 2 = 4\"\n}", "2 + 2 = 4", true},
		{`"plain json string reply"`, "plain json string reply", true}, // plain-JSON rung
		{`{"name": "read_file", "arguments": {"path": "x"}}`, "", false},
		// A stray response key alongside a call shape must not hijack the call.
		{`{"name": "read_file", "arguments": {}, "response": "x"}`, "", false},
		{"just prose", "", false},
		{`{"response": 4}`, "", false},
		{`{"response": "  "}`, "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := UnwrapConstrainedProse(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("UnwrapConstrainedProse(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestUnwrapResponseEnvelopeIsExact(t *testing.T) {
	if got, ok := UnwrapResponseEnvelope(`{"response":"clean prose"}`); !ok || got != "clean prose" {
		t.Fatalf("exact response envelope = (%q, %v)", got, ok)
	}
	for _, content := range []string{
		`{"response":"keep the object","status":"ok"}`,
		`{"name":"read_file","arguments":{}}`,
		`"plain JSON string"`,
	} {
		if got, ok := UnwrapResponseEnvelope(content); ok {
			t.Errorf("ordinary JSON %q was unwrapped as %q", content, got)
		}
	}
}

func TestIsFormatRejection(t *testing.T) {
	if !IsFormatRejection(schemaRejection()) {
		t.Fatal("400 schema conversion failure should read as a format rejection")
	}
	if IsFormatRejection(fmt.Errorf("unexpected status code: 500: internal error")) {
		t.Fatal("5xx is transient, not a format rejection")
	}
	if IsFormatRejection(fmt.Errorf("http request failed: connection reset")) {
		t.Fatal("transport errors are transient, not format rejections")
	}
	if IsFormatRejection(nil) {
		t.Fatal("nil error is not a rejection")
	}
}

func TestConstrainedDecodingSupported(t *testing.T) {
	native := api.OllamaHost{}
	if !ConstrainedDecodingSupported(native) {
		t.Fatal("native Ollama should support constrained decoding")
	}
	openai := api.OllamaHost{}
	openai.SetProvider(api.ProviderOpenAI)
	if ConstrainedDecodingSupported(openai) {
		t.Fatal("OpenAI-compatible providers keep the repair-only path")
	}
	cursor := api.OllamaHost{}
	cursor.SetProvider(api.ProviderCursor)
	if ConstrainedDecodingSupported(cursor) {
		t.Fatal("the cursor agent speaks no tool protocol — never constrain it")
	}
	if !ConstrainedDecodingSupported(&fakeOllama{}) {
		t.Fatal("native Ollama-shaped fake should support constrained decoding")
	}
	if ConstrainedDecodingSupported(&fakeChat{}) {
		t.Fatal("host without provider-kind methods is unknown — stay conservative")
	}
}

func TestRun_ConstrainsActionTurnsAndUnwrapsProse(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	host := &fakeOllama{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"hi"}`),
		textResp(`{"response": "all done"}`), // prose escape branch
	}}
	res, err := Run(context.Background(), host, reg, "do it", Options{Model: "m", ConstrainToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	if seen != "hi" {
		t.Fatalf("tool not dispatched, seen=%q", seen)
	}
	if res.Output != "all done" {
		t.Fatalf("prose envelope should be unwrapped, got %q", res.Output)
	}
	if len(host.requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(host.requests))
	}
	for i, req := range host.requests {
		if len(req.Format) == 0 {
			t.Fatalf("request %d should carry a constrained-decoding format", i)
		}
		if !strings.Contains(string(req.Format), "anyOf") || !strings.Contains(string(req.Format), `"echo"`) {
			t.Fatalf("request %d format should be the full union over the turn's tools: %s", i, req.Format)
		}
	}
}

func TestRun_FallbackLadderDegradesAndCaches(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	// Host 400s any schema containing a union; the flat enum rung is accepted.
	host := &fakeOllama{
		responses: []api.ChatResponse{textResp(`{"response": "ok"}`)},
		reject: func(format json.RawMessage) error {
			if strings.Contains(string(format), "anyOf") {
				return schemaRejection()
			}
			return nil
		},
	}
	cache := NewConstraintCache()
	res, err := Run(context.Background(), host, reg, "task", Options{Model: "m", ConstrainToolCalls: true, Constraints: cache})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "ok" {
		t.Fatalf("output = %q", res.Output)
	}
	if len(host.requests) != 2 {
		t.Fatalf("expected probe + degraded retry (2 requests), got %d", len(host.requests))
	}
	if !strings.Contains(string(host.requests[0].Format), "anyOf") {
		t.Fatal("first request should probe the full union")
	}
	if strings.Contains(string(host.requests[1].Format), "anyOf") || !strings.Contains(string(host.requests[1].Format), `"enum"`) {
		t.Fatalf("retry should use the flat enum rung, got %s", host.requests[1].Format)
	}

	// A later run against the same model+host starts at the working rung —
	// the rejection is not re-probed.
	host2 := &fakeOllama{
		responses: []api.ChatResponse{textResp(`{"response": "ok again"}`)},
		reject: func(format json.RawMessage) error {
			if strings.Contains(string(format), "anyOf") {
				return schemaRejection()
			}
			return nil
		},
	}
	if _, err := Run(context.Background(), host2, reg, "task", Options{Model: "m", ConstrainToolCalls: true, Constraints: cache}); err != nil {
		t.Fatal(err)
	}
	if len(host2.requests) != 1 {
		t.Fatalf("cached rung should avoid re-probing, got %d requests", len(host2.requests))
	}
	if strings.Contains(string(host2.requests[0].Format), "anyOf") {
		t.Fatal("cached rung should skip straight past the rejected union")
	}
}

func TestRun_FallbackLadderBottomsOutUnconstrained(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	// Host rejects every non-empty format; the final retry sends none at all.
	host := &fakeOllama{
		responses: []api.ChatResponse{textResp("plain answer")},
		reject: func(format json.RawMessage) error {
			if len(format) > 0 {
				return schemaRejection()
			}
			return nil
		},
	}
	res, err := Run(context.Background(), host, reg, "task", Options{Model: "m", ConstrainToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "plain answer" {
		t.Fatalf("output = %q", res.Output)
	}
	if len(host.requests) != 4 {
		t.Fatalf("expected union + enum + json + unconstrained (4 requests), got %d", len(host.requests))
	}
	if len(host.requests[3].Format) != 0 {
		t.Fatalf("last-ditch retry must be unconstrained, got %s", host.requests[3].Format)
	}
}

func TestRun_NoConstraintWhenDisabledOrUnsupported(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)

	// Disabled: no format even on a native-Ollama host.
	host := &fakeOllama{responses: []api.ChatResponse{textResp("done")}}
	if _, err := Run(context.Background(), host, reg, "task", Options{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if len(host.requests[0].Format) != 0 {
		t.Fatal("ConstrainToolCalls off must not send a format")
	}

	// Enabled but the host's provider kind is unknown: stay unconstrained.
	plain := &fakeChat{responses: []api.ChatResponse{textResp("done")}}
	if _, err := Run(context.Background(), plain, reg, "task", Options{Model: "m", ConstrainToolCalls: true}); err != nil {
		t.Fatal(err)
	}
	_ = seen
}

func TestRun_FinalizeIsNeverConstrained(t *testing.T) {
	var seen string
	reg := echoRegistry(&seen)
	// Repeat one call until the stuck-guard breaks to the tool-less finalize.
	host := &fakeOllama{responses: []api.ChatResponse{
		toolResp("echo", `{"text":"same"}`),
		toolResp("echo", `{"text":"same"}`),
		toolResp("echo", `{"text":"same"}`),
		textResp("final report"),
	}}
	res, err := Run(context.Background(), host, reg, "task", Options{Model: "m", MaxSteps: 8, ConstrainToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "final report" {
		t.Fatalf("output = %q", res.Output)
	}
	if len(host.requests) != 4 {
		t.Fatalf("expected 3 action rounds + finalize, got %d requests", len(host.requests))
	}
	for i, req := range host.requests[:3] {
		if len(req.Format) == 0 {
			t.Fatalf("action round %d should be constrained", i)
		}
	}
	if last := host.requests[3]; len(last.Format) != 0 || len(last.Tools) != 0 {
		t.Fatal("the forced final synthesis is a prose turn: no tools, no format")
	}
}

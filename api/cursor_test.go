package api

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeCursorAgent writes a stub binary that records the argv it was called with
// and replays a canned stream-json response, so the transport is tested without
// the real CLI installed.
func fakeCursorAgent(t *testing.T, script string) (bin, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "fake-cursor-agent")
	argvLog = filepath.Join(dir, "argv")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvLog + "\n" + script + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argvLog
}

func cursorHost(bin string) OllamaHost {
	h := OllamaHost{}
	h.SetURI(bin)
	h.SetProvider(ProviderCursor)
	return h
}

// In --plan mode the assistant/result stream is only progress narration; the
// plan itself arrives as a createPlanRequestQuery. Reading the wrong one is what
// made a plan naming files look like prose naming none.
func TestCursorTakesThePlanNotTheNarration(t *testing.T) {
	bin, argvLog := fakeCursorAgent(t, `
cat <<'EOF'
{"type":"system","subtype":"init","apiKeySource":"login"}
{"type":"thinking","subtype":"delta","text":"weighing it","timestamp_ms":1}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Exploring tui/route.go."}]},"timestamp_ms":2}
{"type":"interaction_query","subtype":"request","query_type":"createPlanRequestQuery","query":{"id":1,"createPlanRequestQuery":{"args":{"plan":"# Plan\n\n**File:** tui/route.go\n\nChange line 544."}}}}
{"type":"interaction_query","subtype":"response","query_type":"createPlanRequestQuery","response":{"id":1}}
{"type":"result","subtype":"success","is_error":false,"result":"Exploring tui/route.go."}
EOF`)

	resp, errs := cursorHost(bin).ContinuousChat(context.Background(), ChatRequest{
		Model:    "auto",
		Messages: []Message{{Role: "user", Content: "plan it"}},
	})
	var content, thinking string
	var done bool
	for c := range resp {
		content += c.Message.Content
		thinking += c.Message.Thinking
		done = done || c.Done
	}
	if err := <-errs; err != nil {
		t.Fatalf("stream error: %v", err)
	}

	if !strings.Contains(content, "tui/route.go") || !strings.Contains(content, "line 544") {
		t.Errorf("content = %q, want the plan from createPlanRequestQuery", content)
	}
	// Narration must not land in the transcript, or checkPlan sees prose with no
	// file paths and refuses the handoff.
	if strings.Contains(content, "Exploring") {
		t.Errorf("content = %q, want narration kept out of it", content)
	}
	if !strings.Contains(thinking, "Exploring") || !strings.Contains(thinking, "weighing it") {
		t.Errorf("thinking = %q, want narration and reasoning on the ticker", thinking)
	}
	if !done {
		t.Error("no terminal chunk — the turn would never end")
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(argv)), "\n")
	for _, want := range []string{"-p", "--plan", "--output-format", "stream-json", "--model", "auto"} {
		if !slices.Contains(args, want) {
			t.Errorf("argv %v is missing %q", args, want)
		}
	}
	// With --stream-partial-output every message is sent as fragments AND then
	// repeated whole, with a timestamp on both, so answers came out doubled.
	if slices.Contains(args, "--stream-partial-output") {
		t.Error("argv passes --stream-partial-output, which duplicates every answer")
	}
	if slices.Contains(args, "-m") {
		t.Error("argv uses -m, which the CLI rejects")
	}
	for _, forbidden := range []string{"--force", "-f", "--yolo"} {
		if slices.Contains(args, forbidden) {
			t.Fatalf("argv contains %q — the agent could write files unchecked", forbidden)
		}
	}
}

// A clarifying question is auto-skipped headlessly, so without surfacing it the
// turn looks like it silently produced nothing.
func TestCursorSurfacesSkippedQuestion(t *testing.T) {
	bin, _ := fakeCursorAgent(t, `
cat <<'EOF'
{"type":"interaction_query","subtype":"request","query_type":"askQuestionInteractionQuery","query":{"id":0,"askQuestionInteractionQuery":{"args":{"title":"Which one-line change?"}}}}
{"type":"result","subtype":"success","is_error":false,"result":"Looked around."}
EOF`)
	resp, errs := cursorHost(bin).ContinuousChat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "x"}},
	})
	var content string
	for c := range resp {
		content += c.Message.Content
	}
	if err := <-errs; err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if !strings.Contains(content, "Which one-line change?") {
		t.Errorf("content = %q, want the skipped question reported", content)
	}
	if !strings.Contains(content, "Looked around.") {
		t.Errorf("content = %q, want the narration kept as context", content)
	}
}

// --trust is opt-in: it marks the working directory trusted in Cursor without
// asking, so it must never be passed unless the provider enabled it.
func TestCursorTrustIsOptIn(t *testing.T) {
	run := func(t *testing.T, trust bool) []string {
		t.Helper()
		bin, argvLog := fakeCursorAgent(t, `echo '{"type":"result","result":"ok"}'`)
		h := cursorHost(bin)
		h.SetTrustWorkspace(trust)
		resp, errs := h.ContinuousChat(context.Background(), ChatRequest{
			Messages: []Message{{Role: "user", Content: "x"}},
		})
		for range resp {
		}
		if err := <-errs; err != nil {
			t.Fatalf("stream error: %v", err)
		}
		argv, err := os.ReadFile(argvLog)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(argv)), "\n")
	}

	if args := run(t, false); slices.Contains(args, "--trust") {
		t.Errorf("argv %v passed --trust without the provider opting in", args)
	}
	if args := run(t, true); !slices.Contains(args, "--trust") {
		t.Errorf("argv %v is missing --trust after opting in", args)
	}
	// --plan is not opt-in; it is the read-only boundary and always applies.
	for _, trust := range []bool{false, true} {
		if args := run(t, trust); !slices.Contains(args, "--plan") {
			t.Errorf("argv %v dropped --plan (trust=%v)", args, trust)
		}
	}
}

// The workspace-trust abort has to say how to fix it. The CLI's own advice is to
// pass --yolo or -f, which would also grant write and shell access.
func TestCursorTrustAbortIsActionable(t *testing.T) {
	bin, _ := fakeCursorAgent(t, `printf '\n  Workspace Trust Required\n  Do you trust this directory?\n' >&2; exit 1`)
	_, errs := cursorHost(bin).ContinuousChat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "x"}},
	})
	err := <-errs
	if err == nil {
		t.Fatal("expected an error")
	}
	got := err.Error()
	if !strings.Contains(got, "Trust") || !strings.Contains(got, "/provider") {
		t.Errorf("error = %q, want it to point at the Trust setting", got)
	}
	if !strings.Contains(got, "yolo") {
		t.Errorf("error = %q, want it to warn against the CLI's --yolo suggestion", got)
	}
}

// An error result must surface as an error, not as an empty successful turn.
func TestCursorResultErrorSurfaces(t *testing.T) {
	bin, _ := fakeCursorAgent(t, `echo '{"type":"result","subtype":"error","is_error":true,"result":"rate limited"}'`)
	_, errs := cursorHost(bin).ContinuousChat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "x"}},
	})
	err := <-errs
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error = %v, want the CLI's failure reason", err)
	}
}

// A run with no plan and no question still has to deliver something rather than
// ending the turn empty.
func TestCursorFallsBackToNarration(t *testing.T) {
	bin, _ := fakeCursorAgent(t, `echo '{"type":"result","result":"only here"}'`)
	resp, errs := cursorHost(bin).ContinuousChat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "x"}},
	})
	var content string
	for c := range resp {
		content += c.Message.Content
	}
	if err := <-errs; err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if content != "only here" {
		t.Errorf("content = %q, want the result event's text", content)
	}
}

func TestCursorReportsFailures(t *testing.T) {
	t.Run("missing binary names the fix", func(t *testing.T) {
		h := cursorHost(filepath.Join(t.TempDir(), "definitely-not-here"))
		_, errs := h.ContinuousChat(context.Background(), ChatRequest{
			Messages: []Message{{Role: "user", Content: "x"}},
		})
		err := <-errs
		if err == nil {
			t.Fatal("expected an error")
		}
		// Both install names, so the message is actionable whichever one the
		// user's setup provides.
		for _, want := range []string{"cursor-agent", "agent", "cursor.com/docs/cli"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, missing %q", err, want)
			}
		}
	})

	t.Run("nonzero exit surfaces stderr", func(t *testing.T) {
		bin, _ := fakeCursorAgent(t, `echo "not logged in" >&2; exit 1`)
		_, errs := cursorHost(bin).ContinuousChat(context.Background(), ChatRequest{
			Messages: []Message{{Role: "user", Content: "x"}},
		})
		err := <-errs
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "not logged in") {
			t.Errorf("error = %q, want the CLI's own message", err)
		}
	})
}

func TestCursorListsModels(t *testing.T) {
	// Verbatim `agent --list-models` output: a header, a blank line, then
	// "<id> - <Description>". Every model line contains spaces, which an earlier
	// parser used as the reason to skip it — so nothing was ever found.
	bin, _ := fakeCursorAgent(t, `
cat <<'EOF'
Available models

auto - Auto (default)
gpt-5.3-codex-high - Codex 5.3 High
claude-opus-5-thinking-high - Opus 5 1M Thinking
composer-2.5 - Composer 2.5
EOF`)
	list, err := cursorHost(bin).GetModelList()
	if err != nil {
		t.Fatalf("GetModelList failed: %v", err)
	}
	var got []string
	for _, mo := range list.Models {
		got = append(got, mo.Name)
	}
	want := []string{"auto", "gpt-5.3-codex-high", "claude-opus-5-thinking-high", "composer-2.5"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("models = %v, want %v", got, want)
	}
}

// The key must not reach the process list, where any local user can read it.
func TestCursorKeyGoesThroughTheEnvironment(t *testing.T) {
	bin, argvLog := fakeCursorAgent(t, `echo '{"type":"result","result":"'"$CURSOR_API_KEY"'"}'`)
	h := cursorHost(bin)
	h.SetAPIKey("secret-key")

	resp, errs := h.ContinuousChat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "x"}},
	})
	var content string
	for c := range resp {
		content += c.Message.Content
	}
	if err := <-errs; err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if content != "secret-key" {
		t.Errorf("CURSOR_API_KEY reached the child as %q, want secret-key", content)
	}
	argv, _ := os.ReadFile(argvLog)
	if strings.Contains(string(argv), "secret-key") {
		t.Error("the API key appears in argv, where any local user can read it")
	}
}

func TestCursorPromptLabelsRoles(t *testing.T) {
	got := cursorPrompt([]Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "second"},
		{Role: "assistant", Content: "   "}, // empty after trimming: dropped
	})
	for _, want := range []string{"be brief", "[user]: first", "[you, earlier]: ok", "[user]: second"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "[you, earlier]") != 1 {
		t.Error("an empty assistant message was included")
	}
}

// The Cursor CLI installs as `cursor-agent` on some setups and plain `agent` on
// others, so whichever is present has to work without configuration.
func TestCursorResolvesEitherName(t *testing.T) {
	var h OllamaHost
	h.SetProvider(ProviderCursor)

	t.Run("both names are tried", func(t *testing.T) {
		if got := h.cursorCommands(); !slices.Contains(got, "agent") || !slices.Contains(got, "cursor-agent") {
			t.Errorf("candidates = %v, want both install names", got)
		}
	})

	t.Run("explicit path is the only candidate", func(t *testing.T) {
		var explicit OllamaHost
		explicit.SetProvider(ProviderCursor)
		explicit.SetURI("/opt/custom/agent")
		if got := explicit.cursorCommands(); len(got) != 1 || got[0] != "/opt/custom/agent" {
			t.Errorf("candidates = %v, want just the configured path", got)
		}
	})

	// The real fallback: the first name doesn't exist, the second does, and the
	// run has to succeed rather than error on the first.
	t.Run("falls through to the name that works", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, cursorCommandCandidates[len(cursorCommandCandidates)-1])
		script := "#!/bin/sh\necho '{\"type\":\"result\",\"result\":\"fallback ok\"}'\n"
		if err := os.WriteFile(real, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir) // only the second candidate exists

		var host OllamaHost
		host.SetProvider(ProviderCursor)
		resp, errs := host.ContinuousChat(context.Background(), ChatRequest{
			Messages: []Message{{Role: "user", Content: "x"}},
		})
		var content string
		for c := range resp {
			content += c.Message.Content
		}
		if err := <-errs; err != nil {
			t.Fatalf("did not fall through to the working binary: %v", err)
		}
		if content != "fallback ok" {
			t.Errorf("content = %q, want the second candidate's output", content)
		}
	})
}

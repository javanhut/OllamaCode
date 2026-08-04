package api

import (
	"context"
	"os"
	"path/filepath"
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

func TestCursorStreamsAssistantText(t *testing.T) {
	bin, argvLog := fakeCursorAgent(t, `
cat <<'EOF'
{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"text":"Plan: "}]}}
{"type":"assistant","message":{"content":[{"text":"edit route.go"}]}}
{"type":"tool_call","subtype":"completed"}
{"type":"result","result":"Plan: edit route.go","duration_ms":12}
EOF`)

	resp, errs := cursorHost(bin).ContinuousChat(context.Background(), ChatRequest{
		Model:    "claude-sonnet-4",
		Messages: []Message{{Role: "user", Content: "plan it"}},
	})
	var content string
	var done bool
	for c := range resp {
		content += c.Message.Content
		done = done || c.Done
	}
	if err := <-errs; err != nil {
		t.Fatalf("stream error: %v", err)
	}

	// The result event repeats the full answer; emitting it after the deltas
	// would duplicate the whole plan.
	if content != "Plan: edit route.go" {
		t.Errorf("content = %q, want the deltas once", content)
	}
	if !done {
		t.Error("no terminal chunk — the turn would never end")
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(argv)), "\n")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-p", "--output-format", "stream-json", "-m", "claude-sonnet-4"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %v is missing %q", args, want)
		}
	}
	// The safety property this whole provider rests on: without --force the CLI
	// proposes edits instead of applying them, so a routed planning model cannot
	// write to the repo behind OllamaCode's approval prompts and /undo.
	for _, forbidden := range []string{"--force", "-f", "--yolo"} {
		for _, a := range args {
			if a == forbidden {
				t.Fatalf("argv contains %q — the agent could write files unchecked", forbidden)
			}
		}
	}
	if last := args[len(args)-1]; !strings.Contains(last, "plan it") {
		t.Errorf("last arg = %q, want the flattened prompt", last)
	}
}

// A run that produces no deltas must still deliver the answer from the result
// event rather than ending the turn empty.
func TestCursorFallsBackToResultEvent(t *testing.T) {
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
	bin, _ := fakeCursorAgent(t, `
cat <<'EOF'
Available models:
  - claude-sonnet-4
  - gpt-5
  composer-1
EOF`)
	list, err := cursorHost(bin).GetModelList()
	if err != nil {
		t.Fatalf("GetModelList failed: %v", err)
	}
	var got []string
	for _, mo := range list.Models {
		got = append(got, mo.Name)
	}
	want := []string{"claude-sonnet-4", "gpt-5", "composer-1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("models = %v, want %v (header and bullets stripped)", got, want)
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
func TestCursorCommandResolvesEitherName(t *testing.T) {
	stub := func(t *testing.T, names ...string) string {
		t.Helper()
		dir := t.TempDir()
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	var h OllamaHost
	h.SetProvider(ProviderCursor)

	t.Run("plain agent", func(t *testing.T) {
		t.Setenv("PATH", stub(t, "agent"))
		if got := h.cursorCommand(); got != "agent" {
			t.Errorf("cursorCommand = %q, want agent", got)
		}
	})

	t.Run("cursor-agent", func(t *testing.T) {
		t.Setenv("PATH", stub(t, "cursor-agent"))
		if got := h.cursorCommand(); got != "cursor-agent" {
			t.Errorf("cursorCommand = %q, want cursor-agent", got)
		}
	})

	t.Run("both present prefers the unambiguous name", func(t *testing.T) {
		t.Setenv("PATH", stub(t, "agent", "cursor-agent"))
		if got := h.cursorCommand(); got != "cursor-agent" {
			t.Errorf("cursorCommand = %q, want cursor-agent", got)
		}
	})

	t.Run("neither present still names something concrete", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if got := h.cursorCommand(); got == "" {
			t.Error("returned an empty command; the error would name nothing")
		}
	})

	t.Run("explicit path wins over PATH", func(t *testing.T) {
		t.Setenv("PATH", stub(t, "cursor-agent"))
		var explicit OllamaHost
		explicit.SetProvider(ProviderCursor)
		explicit.SetURI("/opt/custom/agent")
		if got := explicit.cursorCommand(); got != "/opt/custom/agent" {
			t.Errorf("cursorCommand = %q, want the configured path", got)
		}
	})
}

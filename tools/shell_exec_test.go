package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSecretEnvNamesAreScrubbed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		secret bool
	}{
		{"OLLAMA_API_KEY", true},
		{"CURSOR_API_KEY", true},
		{"GH_TOKEN", true},
		{"aws_secret_access_key", true}, // lower case: matching is case-insensitive
		{"PGPASSWORD", true},
		{"PATH", false},
		{"HOME", false},
		{"LANG", false},
		{"TERM", false},
		{"TMPDIR", false},
		{"SSH_AUTH_SOCK", false}, // guards against anyone adding AUTH to the list
	} {
		if got := isSecretEnvName(tc.name); got != tc.secret {
			t.Errorf("isSecretEnvName(%q) = %v, want %v", tc.name, got, tc.secret)
		}
	}
}

// The end-to-end proof: a model running `env` cannot read the user's
// credentials out of the environment, but ordinary variables still reach the
// command or half of everything breaks.
func TestRunShellDropsSecretsFromChildEnvironment(t *testing.T) {
	t.Setenv("OCODE_FAKE_API_KEY", "planted-secret-value")
	t.Setenv("OCODE_FAKE_PLAIN", "kept-value")

	out, err := RunShellTool().Handler(context.Background(), jailArgs(t, map[string]string{"command": "env"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "planted-secret-value") {
		t.Error("run_shell leaked a credential into command output")
	}
	if !strings.Contains(out, "PATH=") {
		t.Errorf("PATH must survive the scrub or every command breaks:\n%s", out)
	}
	if !strings.Contains(out, "OCODE_FAKE_PLAIN=kept-value") {
		t.Errorf("scrub must be surgical, not a wipe:\n%s", out)
	}
}

// The scrub is worthless if a dedicated tool hands the same variable over:
// env_list used to return os.Environ() verbatim, straight into the transcript,
// the model's context on every later request, and the trace file.
func TestEnvToolsRespectTheScrub(t *testing.T) {
	t.Setenv("OCODE_TEST_API_KEY", "sk-should-not-appear")
	t.Setenv("DATABASE_URL", "postgres://app:s3cr3t@db.internal/app")
	t.Setenv("OCODE_TEST_ORDINARY", "keep-me")

	listed, err := ListEnvTool().Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listed, "sk-should-not-appear") || strings.Contains(listed, "s3cr3t") {
		t.Error("env_list leaked a credential")
	}
	if !strings.Contains(listed, "keep-me") {
		t.Error("env_list dropped an ordinary variable")
	}

	got, err := GetEnvTool().Handler(context.Background(), json.RawMessage(`{"key":"OCODE_TEST_API_KEY"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "sk-should-not-appear") {
		t.Errorf("env_get leaked a credential: %q", got)
	}
}

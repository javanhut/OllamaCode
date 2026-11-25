package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/tools"
)

func TestVerifyTrackerStuckSignal(t *testing.T) {
	tr := VerifyTracker{Max: 4}
	if adv, stop := tr.OnFailure("go build", "a.go:1:1: undefined: x"); stop || strings.Contains(adv, "SAME ERROR") {
		t.Fatalf("first failure is ordinary: stop=%v %s", stop, adv)
	}
	if adv, stop := tr.OnFailure("go build", "a.go:9:3: undefined: x"); stop || !strings.Contains(adv, "SAME ERROR") {
		t.Fatalf("second identical failure must ask for a new approach: stop=%v %s", stop, adv)
	}
	if adv, stop := tr.OnFailure("go build", "a.go:4:1: undefined: x"); !stop || !strings.Contains(adv, "STUCK") {
		t.Fatalf("third identical failure must stop: stop=%v %s", stop, adv)
	}
	if !tr.Exhausted() {
		t.Fatal("tracker must report exhausted after stop")
	}
	tr = VerifyTracker{Max: 2}
	tr.OnFailure("x", "error one")
	if _, stop := tr.OnFailure("x", "error two"); !stop {
		t.Fatal("the attempt cap still applies to changing errors")
	}
}

// TestRun_HeadlessFlagsWeakenedTests: a non-interactive run whose check passes
// only because the model deleted a test is handed back once to account for it,
// and the warning is reported in the result.
func TestRun_HeadlessFlagsWeakenedTests(t *testing.T) {
	dir := inTempWorkspace(t)
	orig := "package a\nfunc TestA(t *testing.T) { t.Fatal(1) }\nfunc TestB(t *testing.T) { t.Fatal(2) }\n"
	if err := os.WriteFile(filepath.Join(dir, "a_test.go"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	weakened := "package a\nfunc TestA(t *testing.T) { t.Fatal(1) }\n"
	host := &fakeChat{responses: []api.ChatResponse{
		toolResp("read_file", `{"path":"a_test.go"}`),
		toolResp("write_file", `{"path":"a_test.go","content":`+jsonString(weakened)+`}`),
		textResp("all tests pass"),
		textResp("I removed TestB because it was failing."),
	}}
	res, err := Run(context.Background(), host, tools.DefaultRegistry(), "make the tests pass", Options{
		Model: "m", Verify: true, VerifyCmd: "true",
		Permissions: []tools.PermissionRule{{Tool: "write_file", Effect: tools.PermissionAllow}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TestIntegrityWarnings) == 0 || !strings.Contains(strings.Join(res.TestIntegrityWarnings, "\n"), "test cases went from 2 to 1") {
		t.Fatalf("weakened test not reported: %v (output %q)", res.TestIntegrityWarnings, res.Output)
	}
	if res.Output != "I removed TestB because it was failing." {
		t.Fatalf("the model must be handed the integrity advice and answer again, got %q", res.Output)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

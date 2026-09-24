package tui

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Verify reviews what write produced, so it must be reachable from write, read
// the diff and run the read-only shell, and never mutate the workspace.
func TestVerifyMode(t *testing.T) {
	if WriteMode.next() != VerifyMode || VerifyMode.next() != ExploreMode {
		t.Fatal("cycle should be write → verify → explore")
	}
	if got, ok := parseMode("verify"); !ok || got != VerifyMode || got.String() != "verify" {
		t.Fatal("verify does not round-trip through parseMode")
	}
	for _, name := range []string{"git_diff", "read_file", "run_shell", "append_session_notes", "switch_mode"} {
		if !toolAllowedInMode(VerifyMode, name) {
			t.Errorf("%s should be allowed in verify", name)
		}
	}
	for _, name := range []string{"write_file", "edit_file", "delete_file", "terminal_open", "git_commit"} {
		if toolAllowedInMode(VerifyMode, name) {
			t.Errorf("%s must not be allowed in verify", name)
		}
	}
	m := &Model{mode: VerifyMode}
	if !m.planGateBlocks(WriteMode) {
		t.Error("verify must hand issues back through plan, not jump to write")
	}
}

func TestRunChecksOnlyInVerify(t *testing.T) {
	for _, mode := range []Mode{ExploreMode, PlanMode, WriteMode, AutoMode} {
		if toolAllowedInMode(mode, "run_checks") {
			t.Errorf("run_checks must not be offered in %s", mode)
		}
	}
	if !toolAllowedInMode(VerifyMode, "run_checks") || !pinnedToolNames(VerifyMode)["run_checks"] {
		t.Error("run_checks must be allowed and pinned in verify")
	}
}

func TestRunChecks(t *testing.T) {
	t.Chdir(t.TempDir())
	write := func(name, body string) {
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\n\ngo 1.21\n")
	write("a.go", "package x\nfunc A() int {return 1}\n")
	args := json.RawMessage(`{"files":["a.go"]}`)

	_, err := runChecks(context.Background(), args)
	if err == nil || !strings.Contains(err.Error(), "unit tests missing for: a.go") || !strings.Contains(err.Error(), "FAIL ! gofmt") {
		t.Fatalf("unformatted, untested code must fail, got %v", err)
	}

	write("a.go", "package x\n\nfunc A() int { return 1 }\n")
	write("a_test.go", "package x\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tif A() != 2 {\n\t\tt.Fatal(\"A\")\n\t}\n}\n")
	if _, err := runChecks(context.Background(), args); err == nil || !strings.Contains(err.Error(), "FAIL go test") {
		t.Fatalf("a failing test must fail, got %v", err)
	}

	write("a_test.go", "package x\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tif A() != 1 {\n\t\tt.Fatal(\"A\")\n\t}\n}\n")
	if out, err := runChecks(context.Background(), args); err != nil {
		t.Fatalf("formatted, tested, passing code must pass, got %v", err)
	} else if !strings.Contains(out, "go project (go)") {
		t.Errorf("report should name the detected project, got %q", out)
	}
}

func TestChecksGateAcceptsOnlyCurrentPass(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("a.go", []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Model{mode: VerifyMode}
	m.noteChanged("a.go")
	m.checksPassed = m.reviewFingerprint()
	if m.maybeChecksGate() != nil || len(m.reviewPaths) != 0 || m.checksPassed != "" {
		t.Fatal("a pass on the current code must end verify and reset the review")
	}

	m.noteChanged("a.go")
	m.checksPassed = m.reviewFingerprint()
	if err := os.WriteFile("a.go", []byte("package y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.checksNudges = maxVerifyAttempts
	if m.maybeChecksGate() != nil || len(m.reviewPaths) == 0 {
		t.Fatal("a pass on stale code must not be accepted")
	}
}

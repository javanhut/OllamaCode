package tui

import (
	"strings"
	"testing"
	"time"
)

func TestEnvironmentBlock(t *testing.T) {
	b := environmentBlock()
	t.Logf("environment block:%s", b)
	for _, want := range []string{"# Environment", "Working directory:", "Version control:", "Platform:", "Shell:", "Date:"} {
		if !strings.Contains(b, want) {
			t.Errorf("environment block missing %q:\n%s", want, b)
		}
	}
	// Date, not time: this block is part of the static system prefix, so a value
	// that moved every request would invalidate the KV cache on every turn.
	if want := "- Date: " + time.Now().Format("2006-01-02") + "\n"; !strings.Contains(b, want) {
		t.Errorf("expected %q in:\n%s", want, b)
	}
	for _, clock := range []string{":00", ":01", ":02"} {
		if strings.Contains(b, "Date:") && strings.Contains(strings.SplitN(b, "Date:", 2)[1][:12], clock) {
			t.Errorf("environment block carries a clock time, which busts the KV cache each request:\n%s", b)
		}
	}

	// The VCS line must be one of the two valid forms so what the model is told
	// always matches what the git_* tools run against (tools.DetectVCS).
	if !strings.Contains(b, "ivaldi (NOT git)") && !strings.Contains(b, "Version control: git") {
		t.Errorf("VCS line malformed:\n%s", b)
	}
}

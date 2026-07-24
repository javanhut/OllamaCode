package tui

import (
	"strings"
	"testing"
)

func TestEnvironmentBlock(t *testing.T) {
	b := environmentBlock()
	t.Logf("environment block:%s", b)
	for _, want := range []string{"# Environment", "Working directory:", "Version control:", "Platform:", "Shell:"} {
		if !strings.Contains(b, want) {
			t.Errorf("environment block missing %q:\n%s", want, b)
		}
	}
	// The VCS line must be one of the two valid forms so what the model is told
	// always matches what the git_* tools run against (tools.DetectVCS).
	if !strings.Contains(b, "ivaldi (NOT git)") && !strings.Contains(b, "Version control: git") {
		t.Errorf("VCS line malformed:\n%s", b)
	}
}

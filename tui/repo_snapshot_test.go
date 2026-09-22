package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoSnapshotBlock(t *testing.T) {
	dir := ckptWorkspace(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "lib.go"), []byte("package pkg\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "first commit")
	os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644)

	big := subagentTestModel()
	block := big.repoSnapshotBlock()
	for _, want := range []string{"Repository snapshot", "does NOT update", "first commit", "dirty.txt", "pkg/"} {
		if !strings.Contains(block, want) {
			t.Errorf("snapshot missing %q:\n%s", want, block)
		}
	}
	if again := big.repoSnapshotBlock(); again != block {
		t.Error("snapshot changed between calls; it must stay stable for the KV cache")
	}

	small := subagentTestModel()
	small.profile = ModelProfile{ParamsB: 3}
	if sb := small.repoSnapshotBlock(); strings.Contains(sb, "## Files") || !strings.Contains(sb, "first commit") {
		t.Errorf("small-model snapshot should have commits but no tree:\n%s", sb)
	}

	off := false
	disabled := subagentTestModel()
	disabled.cfg.RepoSnapshot = &off
	if got := disabled.repoSnapshotBlock(); got != "" {
		t.Errorf("repo_snapshot=false still produced:\n%s", got)
	}
}

package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Memory keyed on the basename alone would merge every repo that happens to
// contain an "api" or "server" directory into one store.
func TestWorkspaceKeyDistinguishesSameBasename(t *testing.T) {
	a := workspaceKey("/Users/x/one/api")
	b := workspaceKey("/Users/x/two/api")

	if a == b {
		t.Fatalf("two different paths share a key: %q", a)
	}
	for _, k := range []string{a, b} {
		if !strings.HasPrefix(k, "api-") {
			t.Errorf("key %q lost the recognizable directory name", k)
		}
	}
	if workspaceKey("/Users/x/one/api") != a {
		t.Error("key is not stable across calls")
	}
}

func TestWorkspaceKeySanitizes(t *testing.T) {
	for _, root := range []string{"/tmp/my project", "/tmp/we:rd/na me", "/"} {
		k := workspaceKey(root)
		if strings.ContainsAny(k, "/: ") {
			t.Errorf("workspaceKey(%q) = %q, want a safe filename", root, k)
		}
		if k == "" {
			t.Errorf("workspaceKey(%q) is empty", root)
		}
	}
}

// Launching from a subdirectory must not fork a separate store from the repo it
// belongs to.
func TestWorkspaceRootFindsTheRepo(t *testing.T) {
	root := t.TempDir()
	// t.TempDir can hand back a symlinked path on macOS (/var → /private/var);
	// compare against the resolved form the code will produce.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "internal", "agent")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Chdir(deep)
	if got := workspaceRoot(); got != root {
		t.Errorf("workspaceRoot() = %q, want the repo root %q", got, root)
	}
	if memoryPath(workspaceRoot()) != memoryPath(root) {
		t.Error("a subdirectory resolved to a different memory file than its repo")
	}
}

// Outside a repository the working directory is the workspace.
func TestWorkspaceRootFallsBackToCwd(t *testing.T) {
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Chdir(dir)
	if got := workspaceRoot(); got != dir {
		t.Errorf("workspaceRoot() = %q, want %q", got, dir)
	}
}

// The pre-scoping store is left on disk, not migrated — but it has to be
// findable, or entries collected before scoping just vanish.
func TestLegacyMemoryNotice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacy := legacyMemoryPath()
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}

	ws := memoryPath("/some/workspace")

	t.Run("silent with no legacy file", func(t *testing.T) {
		if got := legacyMemoryNotice(ws); got != "" {
			t.Errorf("notice = %q, want none", got)
		}
	})

	if err := os.WriteFile(legacy, []byte(`{"long_term":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("points at the legacy file", func(t *testing.T) {
		got := legacyMemoryNotice(ws)
		if !strings.Contains(got, legacy) {
			t.Errorf("notice = %q, want it to name %q", got, legacy)
		}
	})

	t.Run("goes quiet once the workspace has memory", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ws, []byte(`{"long_term":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := legacyMemoryNotice(ws); got != "" {
			t.Errorf("notice = %q, want none once this workspace has its own", got)
		}
	})
}

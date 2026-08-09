package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinJail points the jail at root (plus optional allowlisted roots) for the
// duration of a test, then restores the unpinned follow-the-cwd behavior.
// Package tests run sequentially, so mutating the package-level jail state is
// safe as long as every test restores it.
func pinJail(t *testing.T, root string, extra ...string) {
	t.Helper()
	SetWorkspaceRoot(root)
	SetJailAllowlist(extra)
	t.Cleanup(func() {
		SetWorkspaceRoot("")
		SetJailAllowlist(nil)
	})
}

func jailArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestJailAllowsInRootPaths(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)
	t.Chdir(root)

	for _, p := range []string{
		".",
		"relative/file.txt",
		filepath.Join(root, "abs.txt"),
		root,
		// Not-yet-existing file under a not-yet-existing directory: containment
		// is judged on the deepest existing ancestor.
		filepath.Join(root, "newdir", "sub", "new.txt"),
	} {
		if err := jailCheck(p); err != nil {
			t.Errorf("jailCheck(%q) rejected an in-root path: %v", p, err)
		}
	}
}

func TestJailRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)
	t.Chdir(root)

	for _, p := range []string{
		"../escape.txt",
		"../../etc/passwd",
		"/etc/hosts",
		filepath.Join(os.TempDir(), "outside-the-root.txt"),
	} {
		err := jailCheck(p)
		if err == nil {
			t.Errorf("jailCheck(%q) allowed an escape", p)
			continue
		}
		if !strings.Contains(err.Error(), "outside the workspace root") {
			t.Errorf("jailCheck(%q) error should name the confinement, got: %v", p, err)
		}
	}

	// The error must name both the rejected path and the root so the model can
	// correct itself on retry.
	err := jailCheck("/etc/hosts")
	if !strings.Contains(err.Error(), "/etc/hosts") {
		t.Errorf("error should name the rejected path, got: %v", err)
	}
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	if !strings.Contains(err.Error(), resolvedRoot) {
		t.Errorf("error should name the workspace root %q, got: %v", resolvedRoot, err)
	}
}

func TestJailRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // sibling temp dir: inside os.TempDir but outside root
	pinJail(t, root)

	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	// Existing file reached through the symlink: full EvalSymlinks catches it.
	if err := jailCheck(filepath.Join(link, "f.txt")); err == nil {
		t.Error("path through an outward symlink must be rejected")
	}
}

func TestJailAllowlist(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	pinJail(t, root, extra)

	if err := jailCheck(filepath.Join(extra, "ok.txt")); err != nil {
		t.Fatalf("allowlisted root rejected: %v", err)
	}
	if err := jailCheck("/etc/hosts"); err == nil {
		t.Error("non-allowlisted absolute path must stay rejected")
	}

	// A real handler honors the allowlist too.
	_, err := WriteFileTool().Handler(context.Background(), jailArgs(t, map[string]string{
		"path": filepath.Join(extra, "ok.txt"), "content": "x",
	}))
	if err != nil {
		t.Fatalf("write_file into allowlisted root failed: %v", err)
	}
}

func TestJailHomeTildeRejected(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)

	// ~ is not expanded by the tools, but the check judges it against the real
	// home directory — outside this root — instead of a funny relative name.
	if err := jailCheck("~/notes.txt"); err == nil {
		t.Error("~ path outside the root must be rejected")
	}
}

func TestJailFollowsWorkingRootWhenUnpinned(t *testing.T) {
	// No pin: the root is derived from the cwd per call, which is what keeps
	// temp-dir-heavy tests green without any jail-aware setup beyond chdir.
	pinJail(t, "")
	dir := t.TempDir()
	t.Chdir(dir)

	if err := jailCheck(filepath.Join(dir, "f.txt")); err != nil {
		t.Fatalf("path under cwd rejected: %v", err)
	}
	other := t.TempDir()
	if err := jailCheck(filepath.Join(other, "f.txt")); err == nil {
		t.Error("path outside cwd-derived root must be rejected")
	}
}

func TestJailedHandlersRejectEscapes(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)
	t.Chdir(root)
	ctx := context.Background()

	if _, err := WriteFileTool().Handler(ctx, jailArgs(t, map[string]string{
		"path": "../escape.txt", "content": "x",
	})); err == nil {
		t.Error("write_file accepted a ../ escape")
	}
	if _, err := ReadFileTool().Handler(ctx, jailArgs(t, map[string]string{
		"path": "/etc/hosts",
	})); err == nil {
		t.Error("read_file accepted an absolute path outside the root")
	}
	// Both ends of a move are confined, not just the source.
	if _, err := MoveFileTool().Handler(ctx, jailArgs(t, map[string]string{
		"source": "in-root.txt", "destination": "../out.txt",
	})); err == nil {
		t.Error("move_file accepted an escaping destination")
	}
	// run_shell's working_dir is confined; the command string is the sandbox's
	// job, not the jail's.
	if _, err := RunShellTool().Handler(ctx, jailArgs(t, map[string]string{
		"command": "true", "working_dir": "/etc",
	})); err == nil {
		t.Error("run_shell accepted a working_dir outside the root")
	}
}

func TestJailedHandlersAllowInRoot(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)
	t.Chdir(root)
	ctx := context.Background()

	if _, err := WriteFileTool().Handler(ctx, jailArgs(t, map[string]string{
		"path": "note.txt", "content": "hello",
	})); err != nil {
		t.Fatalf("write_file in root: %v", err)
	}
	out, err := ReadFileTool().Handler(ctx, jailArgs(t, map[string]string{"path": "note.txt"}))
	if err != nil || !strings.Contains(out, "hello") {
		t.Fatalf("read_file in root: out=%q err=%v", out, err)
	}
	if _, err := ListDirectoryTool().Handler(ctx, jailArgs(t, map[string]string{"path": "."})); err != nil {
		t.Fatalf("list_directory in root: %v", err)
	}
}

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Workspace confinement for filesystem-touching tools. Until now the only
// boundary was the prompt-time trusted-folder check in the TUI; the tools
// themselves would read or write anywhere on disk. jailCheck is the real
// enforcement point: every path argument is resolved (absolute-ified, cleaned,
// symlinks evaluated — for not-yet-existing paths, the deepest existing
// ancestor) and rejected when it lands outside the workspace root.
//
// Threading: the tools package has no per-session state to hang this on —
// handlers resolve relative paths against the process working directory — so
// the jail follows that same mechanism. The root is the workspace root
// (enclosing repo when there is one, else the cwd), pinned once at TUI
// startup via SetWorkspaceRoot; when nothing is pinned (tests, headless eval)
// it is re-derived from the cwd on every call so it tracks t.Chdir and
// friends. The allowlist is the escape hatch for paths the user explicitly
// sanctions (config "jail_allowlist"); it is process-wide package state
// because the config struct lives in the tui package, which already imports
// tools — importing it back would be a cycle.
//
// "~" is NOT expanded by the tools themselves (os.Open("~/x") has never
// worked here), but a leading ~ IS expanded to the home directory for the
// containment check, so "~/x" is judged against its real location — outside
// the workspace unless the home tree is allowlisted — instead of passing as a
// funny-looking relative directory. Absolute paths outside the root are
// rejected for the same reason. Path comparison is byte-exact after symlink
// resolution (same semantics as the existing trusted-folder check); on
// case-insensitive filesystems a path with wrong-case components is rejected
// rather than silently accepted.

var jailState struct {
	sync.RWMutex
	root  string   // pinned workspace root, already symlink-resolved; "" = derive per call
	extra []string // allowlisted additional roots, already symlink-resolved
}

// SetWorkspaceRoot pins the jail root. Called once from TUI startup. An empty
// root unpins: the root is then derived from the process cwd on each
// jailCheck (tests, headless eval).
func SetWorkspaceRoot(root string) {
	if strings.TrimSpace(root) != "" {
		root = resolveJailPath(root)
	} else {
		root = ""
	}
	jailState.Lock()
	defer jailState.Unlock()
	jailState.root = root
}

// SetJailAllowlist replaces the extra permitted roots (config jail_allowlist).
// Entries are resolved the same way as the root; unreadable entries are kept
// as-is so a typo fails closed (nothing inside them matches) rather than
// silently widening the jail.
func SetJailAllowlist(roots []string) {
	resolved := make([]string, 0, len(roots))
	for _, r := range roots {
		if strings.TrimSpace(r) == "" {
			continue
		}
		resolved = append(resolved, resolveJailPath(expandHome(r)))
	}
	jailState.Lock()
	defer jailState.Unlock()
	jailState.extra = resolved
}

// jailRoots returns every root a path may live under: the workspace root
// first, then the allowlist. The sandbox reuses this as its writable set.
func jailRoots() []string {
	jailState.RLock()
	pinned, extra := jailState.root, append([]string(nil), jailState.extra...)
	jailState.RUnlock()
	root := pinned
	if root == "" {
		// Unpinned: follow the tool's working root, i.e. where relative paths
		// actually resolve. Per-call on purpose — a test that chdirs must not
		// inherit the previous test's root.
		root = resolveJailPath(WorkspaceRoot())
	}
	return append([]string{root}, extra...)
}

// WorkspaceRoot is the directory the jail and per-workspace state key on: the
// enclosing repository when there is one, so launching from a subdirectory
// neither forks state nor jails sibling packages away, else the working
// directory.
func WorkspaceRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	for {
		for _, marker := range []string{".ivaldi", ".git"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // reached the filesystem root
		}
		dir = parent
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return ""
}

// jailCheck resolves p and rejects it when it lands outside every jail root.
// It is a gate, not a rewrite: the caller keeps using its original string for
// I/O and for echoing back to the model, so relative paths stay relative in
// tool output. The gap between checked path and used path is single-threaded
// within a handler call.
func jailCheck(p string) error {
	if p == "" {
		return fmt.Errorf("path is required")
	}
	abs := expandHome(p)
	if !filepath.IsAbs(abs) {
		// Join against the cwd, not the root: that is where the OS will
		// resolve the relative path when the handler uses it.
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("cannot resolve %q: %w", p, err)
		}
		abs = filepath.Join(cwd, abs)
	}
	resolved := resolveJailPath(abs)
	for _, root := range jailRoots() {
		if pathWithin(root, resolved) {
			return nil
		}
	}
	roots := jailRoots()
	return fmt.Errorf("path %q resolves to %q, outside the workspace root %q — file access is confined to the workspace; retry with a path inside it, or ask the user to add the location to jail_allowlist in config",
		p, resolved, roots[0])
}

// JailCheck exposes the workspace-containment check to callers outside this
// package that read files directly instead of going through a tool handler —
// currently the TUI's @file mention expansion. Same semantics as jailCheck:
// the caller keeps using its original path string for I/O.
func JailCheck(p string) error { return jailCheck(p) }

// pathWithin reports whether resolved lies at or under root. Both must
// already be absolute and symlink-resolved.
func pathWithin(root, resolved string) bool {
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolveJailPath absolute-ifies, cleans, and resolves symlinks. For a path
// that does not exist yet (the common write_file case) the deepest existing
// ancestor is resolved and the missing tail re-appended, so a symlinked
// parent cannot smuggle a new file outside the root.
func resolveJailPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	var tail []string
	for cur := abs; ; {
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs // not even the filesystem root resolved — take the cleaned path
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for _, seg := range tail {
				resolved = filepath.Join(resolved, seg)
			}
			return resolved
		}
	}
}

// expandHome maps "~/x" and "~" to the user's home directory. Anything else
// ("~other/x") is left untouched and will simply fail containment or I/O.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

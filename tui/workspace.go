package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// Long-term memory is injected into every prompt, so a single global store means
// every project's facts show up in every other project's context. These resolve
// a per-workspace path instead.

// legacyMemoryPath is the pre-scoping global store. It is never read or written
// any more, but it is deliberately left on disk rather than migrated: dumping
// one project's accumulated memory into whichever workspace happened to open
// first would spread exactly the noise this scoping removes.
func legacyMemoryPath() string {
	return filepath.Join(os.Getenv("HOME"), ".ollama_code", "user_memory.json")
}

// workspaceRoot is the directory memory is keyed on: the enclosing repository
// when there is one, so launching from a subdirectory doesn't fork a separate
// store, else the working directory.
func workspaceRoot() string {
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

// workspaceKey is a stable, human-recognizable filename for a workspace: its
// directory name plus a hash of the full path, so two checkouts sharing a
// basename ("api" in three repos) don't share a memory store.
func workspaceKey(root string) string {
	sum := sha256.Sum256([]byte(root))
	short := hex.EncodeToString(sum[:4])
	name := filepath.Base(root)
	// Keep the filename tame without losing the ability to recognize it.
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
	if name == "" || name == "-" || name == "." {
		name = "workspace"
	}
	return name + "-" + short
}

// memoryPath is where this workspace's long-term memory lives.
func memoryPath(root string) string {
	return filepath.Join(os.Getenv("HOME"), ".ollama_code", "memory", workspaceKey(root)+".json")
}

// legacyMemoryNotice points at the old global store when this workspace has no
// memory of its own yet, so entries accumulated before scoping don't silently
// disappear behind the new path. Goes quiet as soon as the workspace remembers
// anything, and when there is no legacy file to salvage.
func legacyMemoryNotice(workspaceMemory string) string {
	if _, err := os.Stat(workspaceMemory); err == nil {
		return ""
	}
	legacy := legacyMemoryPath()
	if _, err := os.Stat(legacy); err != nil {
		return ""
	}
	return "memory is per-workspace now — earlier shared memory is still at " + legacy
}

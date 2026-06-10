package backend

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// EnsureUV returns the path to the `uv` executable, installing it via the
// official installer if it isn't already present. uv manages its own Python
// toolchain, so this also sidesteps the system Python version entirely.
func EnsureUV(ctx context.Context) (string, error) {
	if p := findUV(); p != "" {
		return p, nil
	}
	// Install with the official script: it drops `uv` into ~/.local/bin.
	cmd := exec.CommandContext(ctx, "sh", "-c", "curl -LsSf https://astral.sh/uv/install.sh | sh")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("install uv: %w (install it manually: https://docs.astral.sh/uv/)", err)
	}
	if p := findUV(); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("uv not found on PATH after install")
}

// findUV resolves uv from PATH or the well-known install locations the official
// installer uses.
func findUV() string {
	if p, err := exec.LookPath("uv"); err == nil {
		return p
	}
	home := os.Getenv("HOME")
	for _, cand := range []string{
		filepath.Join(home, ".local", "bin", "uv"),
		filepath.Join(home, ".cargo", "bin", "uv"),
		"/opt/homebrew/bin/uv",
		"/usr/local/bin/uv",
	} {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	return ""
}

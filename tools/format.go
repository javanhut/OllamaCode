package tools

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Formatting is opt-in by presence, the same contract as linting: a formatter
// that is not installed is not an error, it simply leaves the bytes alone. Every
// failure path — unknown extension, missing binary, non-zero exit, timeout,
// empty output — returns the caller's bytes unchanged, so a broken or hostile
// formatter can never corrupt a write.
//
// Formatters are driven over stdin/stdout rather than in-place (`-w`): the
// write path needs the resulting bytes in memory anyway for the hash and the
// diff, and a formatter that dies mid-run never gets to touch the file.

// formatTimeout bounds a single formatter run. A wedged formatter must not hang
// the write that invoked it.
const formatTimeout = 5 * time.Second

var formatOn atomic.Bool

func init() {
	// On by default; config format=false is the kill-switch.
	formatOn.Store(true)
}

// SetFormatEnabled is the config kill-switch (format, default true). Wired from
// TUI startup; tests can toggle it directly.
func SetFormatEnabled(on bool) { formatOn.Store(on) }

func formatEnabled() bool { return formatOn.Load() }

// formatter is one stdin->stdout formatting command. args receives the file's
// path because most formatters need it to pick a syntax and resolve project
// config even when the content arrives on stdin.
type formatter struct {
	bin  string
	args func(path string) []string
}

var (
	gofmtFormatter    = formatter{bin: "gofmt"}
	rustfmtFormatter  = formatter{bin: "rustfmt", args: func(string) []string { return []string{"--emit", "stdout"} }}
	ruffFormatter     = formatter{bin: "ruff", args: func(p string) []string { return []string{"format", "--stdin-filename", p, "-"} }}
	prettierFormatter = formatter{bin: "prettier", args: func(p string) []string { return []string{"--stdin-filepath", p} }}
)

// formatters maps a lowercased extension to its formatter. Extensions absent
// from the table are passed through untouched.
var formatters = func() map[string]formatter {
	m := map[string]formatter{
		".go": gofmtFormatter,
		".rs": rustfmtFormatter,
		".py": ruffFormatter,
	}
	for _, ext := range []string{
		".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
		".json", ".css", ".scss", ".less", ".html", ".md", ".yaml", ".yml",
	} {
		m[ext] = prettierFormatter
	}
	return m
}()

// formatBytes returns data formatted by the tool registered for path's
// extension, or data unchanged when formatting is off, unavailable, or fails
// for any reason. It is called on the write path after the syntax gate and
// before os.WriteFile, so the hash and diff reported back to the model describe
// what actually landed on disk.
func formatBytes(path string, data []byte) []byte {
	if !formatEnabled() || len(data) == 0 {
		return data
	}
	f, ok := formatters[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return data
	}
	bin, err := exec.LookPath(f.bin)
	if err != nil {
		return data
	}
	var args []string
	if f.args != nil {
		args = f.args(path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), formatTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = bytes.NewReader(data)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil // diagnostics are the formatter's business, not the model's
	if err := cmd.Run(); err != nil || out.Len() == 0 {
		return data
	}
	return out.Bytes()
}

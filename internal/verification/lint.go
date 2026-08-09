package verification

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Linting is opt-in by presence: a linter that is not installed is not an
// error, it simply produces no diagnostics, and the gate behaves exactly as
// if this file did not exist. Diagnostics are informational — they sharpen
// the repair message after a failed check but never decide pass/fail, so
// pre-existing findings can never trap the model in a repair loop over code
// it did not touch.

const (
	// maxLintDiagnostics caps the lines fed back to the model; lint output on
	// a fresh package can be long and the tail is rarely the useful part.
	maxLintDiagnostics = 20
	// maxLintBytes bounds the formatted diagnostics block.
	maxLintBytes = 2000
)

// lintDiagLine matches the universal `file:line:col: message` shape (golint,
// staticcheck, and most linters agree on it). Output that does not match —
// loader errors, usage text, panics — is dropped rather than shown as if it
// were a diagnostic.
var lintDiagLine = regexp.MustCompile(`^[^\s:]+\.(go|rs|py|ts|tsx|js|jsx):\d+:\d+:`)

// LintCommand returns a linter invocation scoped to the changed files, or
// ok=false when no linter applies — wrong project kind, no relevant changed
// files, or the binary is not installed.
func LintCommand(root string, changed []string) (cmd string, ok bool) {
	if exists(filepath.Join(root, "go.mod")) {
		if _, err := exec.LookPath("staticcheck"); err == nil {
			if pkgs := goChangedPackages(root, changed); len(pkgs) > 0 {
				return "staticcheck " + strings.Join(pkgs, " "), true
			}
		}
	}
	return "", false
}

// Lint runs the available linter scoped to the changed files and returns
// capped "file:line:col: message" diagnostics restricted to files the turn
// actually touched, or "" when no linter is installed, none applies, it found
// nothing, or it failed for a non-diagnostic reason. Shares the caller's
// context so a check that already burned its budget skips linting for free.
func Lint(ctx context.Context, root string, changed []string) string {
	cmd, ok := LintCommand(root, changed)
	if !ok {
		return ""
	}
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	c.Dir = root
	out, err := c.CombinedOutput()
	if err == nil {
		return "" // clean run, no findings
	}
	// Linter output is relative to the working directory; changed paths may be
	// absolute, so bring them to the same form before filtering.
	rel := make([]string, 0, len(changed))
	for _, path := range changed {
		if filepath.IsAbs(path) {
			if r, rerr := filepath.Rel(root, path); rerr == nil {
				path = r
			}
		}
		rel = append(rel, path)
	}
	return FormatLintDiagnostics(string(out), rel)
}

// FormatLintDiagnostics filters raw linter output to diagnostics in changed
// files and caps the result. Exported for the tests; Lint is the entry point.
func FormatLintDiagnostics(out string, changed []string) string {
	touched := map[string]bool{}
	for _, path := range changed {
		touched[filepath.ToSlash(filepath.Clean(path))] = true
	}
	var lines []string
	extra := 0
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if !lintDiagLine.MatchString(ln) {
			continue
		}
		file, _, _ := strings.Cut(ln, ":")
		if len(touched) > 0 && !touched[filepath.ToSlash(filepath.Clean(file))] {
			continue
		}
		if len(lines) >= maxLintDiagnostics {
			extra++
			continue
		}
		lines = append(lines, ln)
	}
	if len(lines) == 0 {
		return ""
	}
	text := strings.Join(lines, "\n")
	if extra > 0 {
		text += fmt.Sprintf("\n… (%d more diagnostics omitted)", extra)
	}
	if len(text) > maxLintBytes {
		text = text[:maxLintBytes] + "\n… (truncated)"
	}
	return text
}

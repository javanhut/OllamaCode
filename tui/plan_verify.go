package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Verification of an offloaded plan. The plan arrives from a model that never
// sees the result of executing it, so it is treated as a proposal to check, not
// an instruction to follow. Two enforcement points, neither of which depends on
// the executing model cooperating:
//
//  1. planCheck, before the handoff — a text that names no files at all is not a
//     plan (it is a question, a refusal, or an error), and never reaches write
//     mode.
//  2. requireReadBeforeEdit, during execution — a file the plan names cannot be
//     edited until it has actually been read this turn.

// planSourceExts are the extensions that make a bare token (no slash) read as a
// file path rather than prose.
var planSourceExts = map[string]bool{
	".go": true, ".rs": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".py": true, ".rb": true, ".java": true, ".c": true, ".h": true, ".cc": true,
	".cpp": true, ".hpp": true, ".cs": true, ".swift": true, ".kt": true, ".php": true,
	".sh": true, ".sql": true, ".toml": true, ".yaml": true, ".yml": true, ".json": true,
	".md": true, ".mod": true, ".txt": true, ".html": true, ".css": true,
}

// planCheck is what a plan claims to touch, and whether those claims hold.
type planCheck struct {
	named   []string // paths the plan refers to, in first-mention order
	missing []string // of those, the ones not on disk
}

// actionable reports whether the text is a plan at all. Naming no file in a
// codebase means it is not one — that is the signal that separates "1. edit
// tui/route.go" from "Which framework are you using?".
func (c planCheck) actionable() bool { return len(c.named) > 0 }

// checkPlan extracts the paths a plan names and stats them.
func checkPlan(plan string) planCheck {
	var c planCheck
	seen := map[string]bool{}
	for _, field := range strings.FieldsFunc(plan, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '"' || r == '`' || r == '\'' || r == '(' || r == ')' || r == '[' || r == ']' || r == '<' || r == '>'
	}) {
		p := planPathToken(field)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		c.named = append(c.named, p)
		if _, err := os.Stat(p); err != nil {
			c.missing = append(c.missing, p)
		}
	}
	return c
}

// planPathToken normalizes one whitespace-separated token into a repo-relative
// path, or "" when it isn't one.
func planPathToken(tok string) string {
	tok = strings.Trim(tok, ".,;:!?*_#")
	if tok == "" || strings.HasPrefix(tok, "-") {
		return ""
	}
	// A URL is not a workspace path.
	if strings.Contains(tok, "://") {
		return ""
	}
	if !strings.Contains(tok, "/") && !planSourceExts[strings.ToLower(filepath.Ext(tok))] {
		return ""
	}
	// Stay inside the workspace: an absolute or escaping path in a plan is not
	// something to go stat, let alone hand to the executing model as verified.
	tok = strings.TrimPrefix(tok, "./")
	if tok == "" || filepath.IsAbs(tok) || strings.HasPrefix(tok, "../") {
		return ""
	}
	return filepath.Clean(tok)
}

// findings renders the check as the note appended to the handoff, naming exactly
// which references the executing model must confirm before trusting them.
func (c planCheck) findings() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Files the plan names: %s.", strings.Join(c.named, ", "))
	if len(c.missing) > 0 {
		fmt.Fprintf(&b, " These do NOT exist yet: %s — either the plan means to create them, or it named the wrong path. Confirm which before you edit anything.", strings.Join(c.missing, ", "))
	}
	b.WriteString(" Read each file you are about to change before changing it; the plan was written without seeing the result of executing it.")
	return b.String()
}

// requireReadBeforeEdit refuses a mutating call against a file the plan named
// but that has not been read this turn. This is the enforced half of verifying
// the plan: the instruction to check first is a prompt the model may ignore,
// whereas this it cannot. Returns "" to allow the call.
//
// Scoped to files the plan itself named, so unrelated edits — and every turn
// that did not come from an offloaded plan — are untouched.
func (m *Model) requireReadBeforeEdit(name string, paths []string) string {
	if !m.planNeedsVerify || len(paths) == 0 {
		return ""
	}
	for _, p := range paths {
		clean := filepath.Clean(strings.TrimPrefix(p, "./"))
		if !m.planPaths[clean] {
			continue // not something the plan claimed; not ours to gate
		}
		if _, err := os.Stat(clean); err != nil {
			continue // a new file can't be read first
		}
		if m.readThisTurn(clean) {
			continue
		}
		return fmt.Sprintf("error: the plan says to change %q but you have not read it this turn. Read it first and confirm the plan still matches the actual code, then retry %s.", clean, name)
	}
	return ""
}

// readThisTurn reports whether any read tool has already opened a path this
// turn, using the same ledger the re-read guard maintains.
func (m *Model) readThisTurn(clean string) bool {
	for key := range m.turnReads {
		if _, path, ok := strings.Cut(key, "\x01"); ok && path == clean {
			return true
		}
	}
	return false
}

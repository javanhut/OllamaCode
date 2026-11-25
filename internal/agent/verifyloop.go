package agent

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/javanhut/ollama_code/internal/verification"
	"github.com/javanhut/ollama_code/tools"
)

// The verification repair loop's policy, shared by the TUI and every
// non-interactive run so the two cannot drift: what to tell the model after a
// failed check, when to stop retrying, and whether a passing check came from
// weakened tests.

// VerifyTracker counts failed checks within one turn.
type VerifyTracker struct {
	Max          int // failures handed back before giving up
	Attempts     int // failed checks so far
	SameFailures int // consecutive failures with an identical signature
	lastSig      string
}

var verifyNumbers = regexp.MustCompile(`\d+(\.\d+)?`)

// ErrorSignature normalizes a failed check's output so the "same" failure
// compares equal across attempts: line/column numbers and durations shift as
// the model edits around an error without fixing it.
func ErrorSignature(output string) string {
	return verifyNumbers.ReplaceAllString(strings.TrimSpace(output), "#")
}

// OnFailure records a failed check and returns the advice for the model.
// stop=true means retrying is pointless: the caller should end the edit loop
// and have the model explain the blocker instead.
//
// Stuck signal: the same failure again means the last fix did nothing.
// Self-repair only helps when an attempt changes something (Olausson et al.,
// 2024), so a second identical failure demands a different approach and a
// third stops instead of spending the remaining attempts on more of the same.
func (t *VerifyTracker) OnFailure(label, detail string) (advice string, stop bool) {
	t.Attempts++
	sig := ErrorSignature(detail)
	if sig != "" && sig == t.lastSig {
		t.SameFailures++
	} else {
		t.SameFailures = 0
	}
	t.lastSig = sig
	switch {
	case t.SameFailures >= 2:
		return fmt.Sprintf("[VERIFICATION STUCK] `%s` failed with the same error %d times in a row; your edits are not changing the outcome:\n\n%s\n\nStop editing. Explain to the user in plain text what is broken, what you tried, and why it did not work — do not claim it works.",
			label, t.SameFailures+1, detail), true
	case t.Max > 0 && t.Attempts >= t.Max:
		return fmt.Sprintf("[VERIFICATION STILL FAILING after %d attempts] `%s` does not pass:\n\n%s\n\nStop editing. Explain to the user in plain text what is broken and why you couldn't fix it — do not claim it works.",
			t.Attempts, label, detail), true
	case t.SameFailures == 1:
		return VerificationFailed(label, detail) +
			"\n\n[SAME ERROR AS LAST ATTEMPT] Your previous fix did not change this failure. Do not repeat the same kind of edit: re-read the code around the error, question your assumption about its cause, and try a different approach.", false
	default:
		return VerificationFailed(label, detail), false
	}
}

// Exhausted reports whether OnFailure has already said stop.
func (t *VerifyTracker) Exhausted() bool {
	return t.SameFailures >= 2 || (t.Max > 0 && t.Attempts >= t.Max)
}

// TestIntegrityAdvice is the message handed back when a passing check came
// with weakened tests (see verification.TestIntegrityWarnings).
func TestIntegrityAdvice(warnings []string) string {
	return "[TEST INTEGRITY] Verification passed, but this turn changed tests or test configuration:\n- " +
		strings.Join(warnings, "\n- ") +
		"\nIf a change was made to get the check to pass rather than because the test itself was wrong, restore the test and fix the code instead. Otherwise, name each of these changes and justify it explicitly in your final message to the user."
}

// TestBaseline remembers the pre-run content of every test file a run is
// about to mutate, for runs without a checkpoint snapshot (headless,
// subagents). Record is called before each mutating tool call.
type TestBaseline struct {
	mu    sync.Mutex
	files map[string]verification.TestFileChange
}

// Record captures the current content of any test path the call will touch,
// once per path.
func (b *TestBaseline) Record(call tools.ToolCall) {
	for _, p := range tools.MutatedPaths(call.Function.Name, call.Function.Arguments) {
		if !verification.IsTestPath(p) {
			continue
		}
		b.mu.Lock()
		if b.files == nil {
			b.files = map[string]verification.TestFileChange{}
		}
		if _, seen := b.files[p]; !seen {
			c := verification.TestFileChange{Path: p}
			if data, err := os.ReadFile(p); err == nil {
				c.Before, c.BeforeExists = string(data), true
			}
			b.files[p] = c
		}
		b.mu.Unlock()
	}
}

// Warnings compares the recorded baselines with the files on disk now.
func (b *TestBaseline) Warnings() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	changes := make([]verification.TestFileChange, 0, len(b.files))
	for _, c := range b.files {
		if data, err := os.ReadFile(c.Path); err == nil {
			c.After, c.AfterExists = string(data), true
		}
		changes = append(changes, c)
	}
	return verification.TestIntegrityWarnings(changes)
}

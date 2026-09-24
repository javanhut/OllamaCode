package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/internal/verification"
	"github.com/javanhut/ollama_code/tools"
)

const checksGateMsg = `[VERIFY GATE] Verify mode cannot pass: run_checks has not passed on the current code. Call run_checks now. If it fails, your verdict is FAIL — record every failure with append_session_notes and call switch_mode("plan", ...). Nothing is approved until formatting, unit-test coverage and the full test suite all pass.`

// noteChanged records a file edit for this turn's compile gate and for the
// next verify-mode review.
func (m *Model) noteChanged(path string) {
	path = filepath.Clean(path)
	if m.turnChangedPaths == nil {
		m.turnChangedPaths = map[string]bool{}
	}
	if m.reviewPaths == nil {
		m.reviewPaths = map[string]bool{}
	}
	m.turnChangedPaths[path] = true
	m.reviewPaths[path] = true
}

func (m *Model) reviewPathList() []string { return slices.Sorted(maps.Keys(m.reviewPaths)) }

func (m *Model) reviewFingerprint() string { return verification.Fingerprint(".", m.reviewPathList()) }

func (m *Model) runChecksTool() tools.Tool {
	return tools.Tool{
		Function: tools.Function{
			Name:        "run_checks",
			Description: "Detect the project's language and package manager, then run its formatting check and full test suite and confirm every changed source file has unit tests. Verify mode cannot pass until this succeeds on the current code.",
			Parameters:  tools.Schema{Type: "object", Properties: map[string]tools.Property{}},
		},
		Handler: runChecks,
	}
}

// runChecks runs every step even after a failure so the reviewer sees all of
// them. args.files is filled in by the harness (invokeToolCmd), never the model.
func runChecks(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Files []string `json:"files"`
	}
	_ = json.Unmarshal(args, &a)
	suite, err := verification.DetectSuite(".")
	if err != nil {
		return "", fmt.Errorf("no test suite detected, so nothing can pass: %w", err)
	}
	var report strings.Builder
	fmt.Fprintf(&report, "%s project (%s)\n", suite.Language, suite.Manager)
	failed := false
	if untested := verification.Untested(".", a.Files); len(untested) > 0 {
		failed = true
		fmt.Fprintf(&report, "FAIL unit tests missing for: %s\n", strings.Join(untested, ", "))
	}
	for _, step := range suite.Steps {
		out, err := tools.NewShellCommand(ctx, step).CombinedOutput()
		if err == nil {
			fmt.Fprintf(&report, "PASS %s\n", step)
			continue
		}
		failed = true
		fmt.Fprintf(&report, "FAIL %s\n%s\n", step, outputTail(out))
	}
	if failed {
		return "", errors.New(report.String())
	}
	return report.String(), nil
}

// maybeChecksGate keeps a verify turn from ending until run_checks has passed
// on the current code; the only other way out is FAIL back to plan.
func (m *Model) maybeChecksGate() tea.Cmd {
	if m.mode != VerifyMode || m.turnStoppedByGuard {
		return nil
	}
	if m.checksPassed != "" && m.checksPassed == m.reviewFingerprint() {
		clear(m.reviewPaths)
		m.checksPassed = ""
		return nil
	}
	if m.checksNudges >= maxVerifyAttempts {
		m.toast = "verify: checks never passed — nothing approved"
		return nil
	}
	m.checksNudges++
	m.history = append(m.history, advisory(checksGateMsg))
	m.busySince = time.Now()
	return m.startStream()
}

package tui

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
)

// maxVerifyAttempts bounds how many times the harness will force the model to
// keep fixing after a failed compile check before it gives up and asks the user.
const maxVerifyAttempts = 4

type verifyDoneMsg struct {
	ok     bool
	label  string
	output string
}

const noCheckChallenge = "[SELF-CHECK] Before you finish: did you ACTUALLY verify this works — run it, build it, or test it — and watch it succeed? If not, do that now with your tools. If you genuinely cannot verify it, say so plainly and list exactly what remains unverified. Do not claim something works without evidence."

func (m *Model) verifyOn() bool {
	return m.cfg.Verify == nil || *m.cfg.Verify
}

// verifyCommand returns the compile/typecheck command for the current project —
// a config override first, then language auto-detection. It deliberately uses
// compile-only checks (not test runners) so it catches the "basic things" like
// type and syntax errors without executing project code.
func (m *Model) verifyCommand() (cmd, label string, ok bool) {
	if c := strings.TrimSpace(m.cfg.VerifyCmd); c != "" {
		return c, "verify", true
	}
	switch {
	case fileExists("go.mod"):
		return "go build ./...", "go build", true
	case fileExists("Cargo.toml"):
		return "cargo check --quiet", "cargo check", true
	case fileExists("tsconfig.json"):
		return "npx --no-install tsc --noEmit", "tsc --noEmit", true
	}
	return "", "", false
}

func fileExists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}

// maybeVerifyGate runs when a file-touching turn tries to end. It returns a
// command to run the compile check (which re-invokes the model on failure), a
// command for a one-time self-check challenge when no objective check exists, or
// nil to let the turn end normally.
func (m *Model) maybeVerifyGate() tea.Cmd {
	if !m.verifyOn() || !m.turnTouchedFiles || m.verifyAttempts >= maxVerifyAttempts {
		return nil
	}
	cmd, label, ok := m.verifyCommand()
	if !ok {
		// No objective check for this project: challenge the model once to prove
		// it verified its work, instead of accepting an unverified "done".
		if !m.challengedThisTurn {
			m.challengedThisTurn = true
			m.history = append(m.history, api.Message{Role: "system", Content: noCheckChallenge})
			m.busySince = time.Now()
			return m.startStream()
		}
		return nil
	}
	m.verifying = true
	m.busySince = time.Now()
	return m.verifyRunCmd(cmd, label)
}

// verifyRunCmd runs the compile check in the background and reports the result.
func (m *Model) verifyRunCmd(command, label string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/bin/sh", "-c", command).CombinedOutput()
		text := strings.TrimSpace(string(out))
		if len(text) > 4000 {
			text = "…\n" + text[len(text)-4000:] // tail: compiler errors cluster at the end
		}
		return verifyDoneMsg{ok: err == nil, label: label, output: text}
	}
}

// endTurnTail handles post-turn housekeeping — proactive compaction and
// dequeuing a queued message. Deferred until verification passes (so we don't
// move on while the build is still broken).
func (m *Model) endTurnTail() []tea.Cmd {
	var cmds []tea.Cmd
	m.lastActivity = time.Now()
	if m.totalTokens > m.contextLimit*9/10 || m.shouldCompact() {
		if c := m.compactContext(); c != nil {
			cmds = append(cmds, c)
		}
	}
	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue = m.queue[1:]
		m.history = append(m.history, api.Message{Role: "user", Content: next})
		m.logActivity("Message (dequeued): " + next)
		m.resetTurnGuards()
		cmds = append(cmds, m.startStream())
		m.refreshTranscript()
		m.viewport.GotoBottom()
	}
	return cmds
}

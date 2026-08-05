package tui

import (
	"context"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
	"github.com/javanhut/ollama_code/internal/verification"
)

// maxVerifyAttempts bounds how many times the harness will force the model to
// keep fixing after a failed compile check before it gives up and asks the user.
const maxVerifyAttempts = 4

type verifyDoneMsg struct {
	ok          bool
	label       string
	command     string
	fingerprint string
	output      string
}

const noCheckChallenge = "[SELF-CHECK] Before you finish: did you ACTUALLY verify this works — run it, build it, or test it — and watch it succeed? If not, do that now with your tools. If you genuinely cannot verify it, say so plainly and list exactly what remains unverified. Do not claim something works without evidence."

func (m *Model) verifyOn() bool {
	return m.cfg.Verify == nil || *m.cfg.Verify
}

// verifyCommand returns a targeted test + compile/typecheck plan. Commands are
// derived only from known manifests and changed paths unless the user supplied
// an explicit override.
func (m *Model) verifyCommand() (cmd, label string, ok bool) {
	plan, ok := verification.Detect(".", m.changedPaths(), m.cfg.VerifyCmd)
	return plan.Command, plan.Label, ok
}

func (m *Model) changedPaths() []string {
	paths := make([]string, 0, len(m.turnChangedPaths))
	for path := range m.turnChangedPaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
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
	return m.verifyRunCmd(cmd, label, verification.Fingerprint(".", m.changedPaths()))
}

// verifyRunCmd runs the compile check in the background and reports the result.
func (m *Model) verifyRunCmd(command, label, fingerprint string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/bin/sh", "-c", command).CombinedOutput()
		text := strings.TrimSpace(string(out))
		if len(text) > 4000 {
			text = "…\n" + text[len(text)-4000:] // tail: compiler errors cluster at the end
		}
		current := verification.Fingerprint(".", m.changedPaths())
		if current != fingerprint {
			err = context.Canceled
			text = "files changed while verification was running; the stale result was discarded"
		}
		if m.trace != nil {
			errText := ""
			if err != nil {
				errText = err.Error()
			}
			_ = m.trace.Record(tracepkg.Event{Kind: "verification", Turn: m.turnGen, Model: m.modelName,
				Result: text, Error: errText, Metadata: map[string]any{"command": command, "label": label, "fingerprint": fingerprint[:12], "ok": err == nil}})
		}
		return verifyDoneMsg{ok: err == nil, label: label, command: command, fingerprint: fingerprint[:12], output: text}
	}
}

// endTurnTail handles post-turn housekeeping — proactive compaction and
// dequeuing a queued message. Deferred until verification passes (so we don't
// move on while the build is still broken).
func (m *Model) endTurnTail() []tea.Cmd {
	var cmds []tea.Cmd
	// Bank the turn's timing, then re-render: the caller already refreshed the
	// transcript before this point, so without a second pass the ⏱ footer (and
	// the /show_thinking block) wouldn't appear until the next redraw.
	atBottom := m.viewport.AtBottom()
	m.finishTurnClock()
	m.refreshTranscript()
	if atBottom {
		m.viewport.GotoBottom()
	}
	m.lastActivity = time.Now()
	if m.totalTokens > m.contextLimit*9/10 || m.shouldCompact() {
		if c := m.compactContext(); c != nil {
			cmds = append(cmds, c)
		}
	}
	if len(m.queue) > 0 {
		cmds = append(cmds, m.dequeueNext())
	}
	return cmds
}

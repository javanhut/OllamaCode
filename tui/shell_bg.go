package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// This file is the push half of background shell jobs (run_shell
// background=true). It mirrors the sub-agent completion flow in
// subagent_bg.go: the job's watcher goroutine in tools/shell_bg.go fires the
// registered notifier on exit, the notifier (registered in NewModel) pushes a
// shellJobDoneMsg onto m.shellJobEvents, and the update loop appends a
// completion notification to the conversation — which wakes the parent when it
// is idle, so the model no longer has to poll shell_output just for status.
// Polling for the full log still works; the notification carries only a tail.

// shellJobDoneMsg is delivered to the update loop when a background shell job
// exits — normally, in error, or killed via shell_output(kill=true).
type shellJobDoneMsg struct {
	id       int
	exitCode int
	err      string
	tail     string
}

// outcome labels the exit for the notification header.
func (m shellJobDoneMsg) outcome() string {
	switch {
	case m.exitCode < 0:
		return "KILLED" // terminated by signal, e.g. shell_output(kill=true)
	case m.exitCode > 0:
		return fmt.Sprintf("FAILED exit=%d", m.exitCode)
	case m.err != "":
		return "FAILED (" + m.err + ")"
	default:
		return fmt.Sprintf("COMPLETE exit=%d", m.exitCode)
	}
}

// toastLine is the short status-surface form of the completion.
func (m shellJobDoneMsg) toastLine() string {
	switch {
	case m.exitCode < 0:
		return fmt.Sprintf("shell job %d killed", m.id)
	case m.exitCode != 0 || m.err != "":
		return fmt.Sprintf("shell job %d failed", m.id)
	default:
		return fmt.Sprintf("shell job %d finished", m.id)
	}
}

// notification renders the completion message injected into the conversation.
// It carries only the tail of the output; the model can poll shell_output for
// the full log.
func (m shellJobDoneMsg) notification() string {
	tail := m.tail
	if tail == "" {
		tail = "(no output)"
	}
	return fmt.Sprintf("[SHELL JOB %d %s] The background shell job you started has exited. Tail of its output (use shell_output with job=%d for the full log):\n\n%s",
		m.id, m.outcome(), m.id, tail)
}

// awaitShellJobEvent blocks until a background shell job exits and delivers it
// as a shellJobDoneMsg. One waiter is armed from Init and re-armed by the
// shellJobDoneMsg handler, so at most one goroutine parks on the channel.
func (m *Model) awaitShellJobEvent() tea.Cmd {
	events := m.shellJobEvents
	if events == nil {
		return nil
	}
	return func() tea.Msg {
		return <-events
	}
}

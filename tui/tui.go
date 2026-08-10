package tui

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
)

// Run starts the TUI with a fresh session. If the previous run died without
// cleaning up, say so and point at --resume rather than silently recovering.
func Run() error {
	m := New()
	if crashedLastRun() {
		if _, err := os.Stat(autosavePath()); err == nil {
			m.toast = "previous session ended unexpectedly — run `ocode --resume` to restore it"
		}
	}
	return runProgram(m)
}

// RunResume starts the TUI restored from the auto-saved session (id == "") or
// from a named session written by /save. Nothing to resume starts fresh.
func RunResume(id string) error {
	m := New()
	m.restoreSession(id)
	return runProgram(m)
}

// runProgram drives the bubbletea loop for an already-built model. It owns the
// crash-marker lifecycle: the marker is written before the loop starts and
// removed only when the loop returns without error, so a crash, kill, or panicky
// exit leaves it behind for the next startup to find.
func runProgram(m *Model) (err error) {
	enableSessionPersistence()
	markSessionRunning()
	defer func() {
		if err == nil {
			clearSessionMarker()
		}
	}()
	p := tea.NewProgram(m)
	m.companionSender = func(msg tea.Msg) { p.Send(msg) }
	defer func() {
		if m.companion != nil {
			_ = m.companion.Close()
			m.companion = nil
		}
		for _, server := range m.mcpServers {
			_ = server.Close()
		}
		m.mcpServers = nil
		if m.trace != nil {
			_ = m.trace.Close()
			m.trace = nil
		}
	}()
	// Last-resort backstop: if Update/View panics, recover so the terminal is
	// restored and the user gets an error instead of a raw stack trace.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered from panic: %v", r)
		}
	}()
	_, err = p.Run()
	return err
}

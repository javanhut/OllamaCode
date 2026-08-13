package tui

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
	"github.com/javanhut/ollama_code/tools"
)

// Run starts the TUI with a fresh session. If the previous run died without
// cleaning up, say so and point at --resume rather than silently recovering.
func Run() error {
	return RunWithDebug("")
}

// RunWithDebug starts a fresh TUI and, when debugPath is non-empty, replaces
// the configured trace destination with a fresh single-session debug log.
func RunWithDebug(debugPath string) error {
	m := New()
	if err := m.enableDebug(debugPath, "tui"); err != nil {
		return err
	}
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
	return RunResumeWithDebug(id, "")
}

// RunResumeWithDebug restores a session with the same opt-in debug logging as
// RunWithDebug.
func RunResumeWithDebug(id, debugPath string) error {
	m := New()
	if err := m.enableDebug(debugPath, "tui-resume"); err != nil {
		return err
	}
	m.restoreSession(id)
	return runProgram(m)
}

func (m *Model) enableDebug(path, surface string) error {
	if path == "" {
		return nil
	}
	if m.trace != nil {
		_ = m.trace.Close()
	}
	recorder, err := tracepkg.OpenFresh(path)
	if err != nil {
		return fmt.Errorf("open debug log %s: %w", path, err)
	}
	m.trace = recorder
	cwd, _ := os.Getwd()
	if err := recorder.Record(tracepkg.Event{Kind: "session_start", Metadata: map[string]any{
		"surface": surface, "working_directory": cwd, "format": "redacted-jsonl", "schema_version": 2,
	}}); err != nil {
		_ = recorder.Close()
		m.trace = nil
		return err
	}
	return nil
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
	// Background shell jobs (run_shell background=true) are detached from the
	// turn that started them; without this they survive every quit path as
	// orphans. Deferred so /quit, ctrl+c, and tea.Quit all reap them.
	defer tools.KillAllBackgroundJobs()
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
			metadata := map[string]any{"reason": "clean_exit"}
			if err != nil {
				metadata["reason"] = "error"
				metadata["error"] = err.Error()
			}
			_ = m.trace.Record(tracepkg.Event{Kind: "session_end", Model: m.modelName, Metadata: metadata})
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

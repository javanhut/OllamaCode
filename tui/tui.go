package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

func Run() (err error) {
	m := New()
	p := tea.NewProgram(m)
	m.companionSender = func(msg tea.Msg) { p.Send(msg) }
	// Last-resort backstop: if Update/View panics, recover so the terminal is
	// restored and the user gets an error instead of a raw stack trace.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered from panic: %v", r)
		}
	}()
	_, err = p.Run()
	if m.companion != nil {
		_ = m.companion.Close()
		m.companion = nil
	}
	return err
}

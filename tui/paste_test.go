package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// A bracketed paste arrives as tea.PasteMsg, which nothing in Update's type
// switch matches — only the per-state forwarding tail delivers it. Adding an
// input to a modal without adding it there drops paste silently, which is how
// the Name and Env fields shipped broken.
func pasteModel() *Model {
	m := routedModel(nil, "small")
	m.urlInput = textinput.New()
	m.keyInput = textinput.New()
	m.nameInput = textinput.New()
	m.envInput = textinput.New()
	m.pullInput = textinput.New()
	ta := textarea.New()
	ta.DynamicHeight = true
	ta.MinHeight = minInputLines
	ta.MaxHeight = maxInputLines
	ta.SetHeight(minInputLines)
	m.input = ta
	m.transcript = &strings.Builder{}
	m.streamBuf = &strings.Builder{}
	m.md = newMarkdownRenderer()
	m.notesMd = newMarkdownRenderer()
	m.cfg.Providers = map[string]providerConfig{"p": {BaseURL: "u"}}
	return m
}

func TestPasteReachesEverySettingsField(t *testing.T) {
	for _, f := range []struct {
		name  string
		focus settingsField
		get   func(*Model) string
	}{
		{"url", settingsFocusURL, func(m *Model) string { return m.urlInput.Value() }},
		{"key", settingsFocusKey, func(m *Model) string { return m.keyInput.Value() }},
		{"name", settingsFocusName, func(m *Model) string { return m.nameInput.Value() }},
		{"env", settingsFocusEnv, func(m *Model) string { return m.envInput.Value() }},
	} {
		t.Run(f.name, func(t *testing.T) {
			m := pasteModel()
			m.state = stateSettings
			m.settingsTarget = 1
			m.focusSettingsField(f.focus)
			m.Update(tea.PasteMsg{Content: "sk-pasted"})
			if got := f.get(m); got != "sk-pasted" {
				t.Errorf("paste dropped: field %s = %q, want the pasted text", f.name, got)
			}
		})
	}
}

func TestPasteReachesChatInput(t *testing.T) {
	m := pasteModel()
	m.state = stateChat
	m.width, m.height, m.ready = 100, 40, true
	m.layout()
	m.input.Focus()
	m.Update(tea.PasteMsg{Content: "hello pasted"})
	if got := m.input.Value(); got != "hello pasted" {
		t.Errorf("paste dropped: chat input = %q, want the pasted text", got)
	}
}

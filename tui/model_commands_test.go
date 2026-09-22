package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
)

func TestModelsCommandOpensPickerWhileLoading(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	m := newSized(t)
	m.input.SetValue("/models")

	mm, cmd := m.updateChatKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(*Model)
	if cmd == nil {
		t.Fatal("/models did not start a model-list request")
	}
	if m.state != stateModelPicker {
		t.Fatalf("state = %v, want the picker to open immediately", m.state)
	}
	if m.statusMsg != "refreshing…" {
		t.Fatalf("status = %q, want a visible loading status", m.statusMsg)
	}
}

func TestClosedModelsPickerIgnoresLateReply(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	m := newSized(t)
	m.state = stateModelPicker
	m.updatePicker(tea.KeyPressMsg{Code: tea.KeyEscape})
	m.Update(modelsLoadedMsg{models: []string{"late"}})

	if m.state != stateChat {
		t.Fatal("a late model-list reply reopened the picker after Esc")
	}
	if len(m.models) != 0 {
		t.Fatalf("late reply replaced the model list: %v", m.models)
	}
}

func TestModelsPickerIgnoresReplyFromPreviousEndpoint(t *testing.T) {
	m := &Model{
		state:            stateModelPicker,
		pickerTarget:     1,
		models:           []string{"current"},
		modelListRequest: 2,
		cfg: config{Providers: map[string]providerConfig{
			"remote": {BaseURL: "https://example.invalid/v1"},
		}},
	}
	m.Update(modelsLoadedMsg{models: []string{"stale"}, from: "", request: 1})
	if len(m.models) != 1 || m.models[0] != "current" {
		t.Fatalf("stale endpoint reply replaced current list: %v", m.models)
	}
}

func TestModelSettingsRequireASelectedModel(t *testing.T) {
	m := &Model{cfg: config{Profiles: map[string]ModelProfile{}}}
	m.modelInfoCommand("ctx 8192")
	if !strings.Contains(m.toast, "no model selected") {
		t.Fatalf("toast = %q, want no-model error", m.toast)
	}
	if len(m.cfg.Profiles) != 0 {
		t.Fatalf("created an empty-name profile: %#v", m.cfg.Profiles)
	}
}

func TestModelSettingsUseProviderQualifiedIdentity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	m := &Model{cfg: config{
		Model: "cursor:auto",
		Providers: map[string]providerConfig{
			"cursor": {Kind: api.ProviderCursor},
		},
		Profiles: map[string]ModelProfile{
			"auto": {NumCtx: 8192},
		},
	}}
	m.host, m.modelName = m.hostForSpec(m.cfg.Model)
	m.profile = ModelProfile{NumCtx: maxContextBudget, SupportsTools: false}

	m.modelInfoCommand("temp 0.4")
	if m.cfg.Profiles["cursor:auto"].Temperature == nil {
		t.Fatal("provider-qualified profile was not written")
	}
	if m.cfg.Profiles["auto"].Temperature != nil {
		t.Fatal("provider setting leaked into the same-named local model")
	}

	m.resolveProfile()
	if m.profile.Temperature == nil || *m.profile.Temperature != 0.4 {
		t.Fatal("Cursor profile override did not survive profile resolution")
	}
	if m.profile.SupportsTools {
		t.Fatal("a cached Cursor profile enabled OllamaCode tool calls")
	}
}

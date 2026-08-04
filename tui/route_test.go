package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
)

// Profiles are pre-seeded so resolveProfile short-circuits on the cache and the
// test never touches the network or the user's config file.
func routedModel(routes map[string]string, dflt string) *Model {
	m := &Model{
		cfg: config{
			Host:   "http://localhost:11434",
			Model:  dflt,
			Routes: routes,
			Profiles: map[string]ModelProfile{
				"small": {NumCtx: 8192, ParamsB: 7, SupportsTools: true},
				"big":   {NumCtx: 131072, ParamsB: 120, SupportsTools: true},
			},
		},
		modelName: dflt,
		notes:     &sessionNotes{}, // plan→write reads notes to inject the plan summary
	}
	m.host = m.defaultHost() // routeIsLoaded compares endpoints, not just names
	return m
}

func TestModelForMode(t *testing.T) {
	tests := []struct {
		name   string
		routes map[string]string
		mode   Mode
		want   string
	}{
		{"unconfigured leaves the model alone", nil, PlanMode, ""},
		{"bound mode uses its binding", map[string]string{"plan": "big"}, PlanMode, "big"},
		{"unbound mode falls back to the default", map[string]string{"plan": "big"}, WriteMode, "small"},
		{"blank binding falls back to the default", map[string]string{"plan": "  "}, PlanMode, "small"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := routedModel(tt.routes, "small").modelForMode(tt.mode); got != tt.want {
				t.Errorf("modelForMode(%v) = %q, want %q", tt.mode, got, tt.want)
			}
		})
	}
}

// The trap this whole feature exists to avoid: swapping the model without
// swapping its context window, so a 128k prompt gets assembled for an 8k model.
func TestApplyRouteSwapsProfileWithModel(t *testing.T) {
	m := routedModel(map[string]string{"plan": "big", "write": "small"}, "small")
	m.applyProfile(m.cfg.Profiles["small"])

	if !m.applyModeTransition(PlanMode, "") {
		t.Fatal("transition to plan was refused")
	}
	if m.modelName != "big" {
		t.Errorf("model = %q, want big", m.modelName)
	}
	if m.contextLimit != 131072 {
		t.Errorf("contextLimit = %d, want 131072 (profile did not follow the model)", m.contextLimit)
	}
	if m.profile.smallModel() {
		t.Error("120B model still classified as small — lean toolset would be applied")
	}

	if !m.applyModeTransition(WriteMode, "") {
		t.Fatal("transition to write was refused")
	}
	if m.modelName != "small" {
		t.Errorf("model = %q, want small", m.modelName)
	}
	if m.contextLimit != 8192 {
		t.Errorf("contextLimit = %d, want 8192", m.contextLimit)
	}
}

// Unbinding the last route must return to the default, not strand the model on
// the route just deleted.
func TestUnbindLastRouteRestoresDefault(t *testing.T) {
	// routeCommand persists via saveConfig — keep the real config out of it.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	m := routedModel(map[string]string{"plan": "big"}, "small")
	m.applyModeTransition(PlanMode, "")
	if m.modelName != "big" {
		t.Fatalf("setup: model = %q, want big", m.modelName)
	}
	m.routeCommand("plan off")
	if m.modelName != "small" {
		t.Errorf("model = %q, want small after unbinding the last route", m.modelName)
	}
	if m.contextLimit != 8192 {
		t.Errorf("contextLimit = %d, want 8192", m.contextLimit)
	}
}

// Unbinding one of several routes must not disturb a still-bound current mode.
func TestUnbindOtherRouteKeepsCurrentBinding(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	m := routedModel(map[string]string{"plan": "big", "write": "small"}, "small")
	m.applyModeTransition(PlanMode, "")
	m.routeCommand("write off")
	if m.modelName != "big" {
		t.Errorf("model = %q, want big (plan is still bound)", m.modelName)
	}
}

func TestRouteScore(t *testing.T) {
	tests := []struct {
		msg      string
		escalate bool
	}{
		// Stay local: mechanical work, however it's phrased.
		{"fix the typo in @readme.md", false},
		{"refactor this function to use a switch", false},
		{"add a test for parseMode", false},
		{"run the tests", false},
		{"port this to use errgroup", false},
		{"update @a.go @b.go @c.go with the new import", false},

		// Escalate: a design question, or a big verb with real breadth.
		{"how should I structure the router?", true},
		{"refactor the auth layer across all the handlers", true},
		{"what's the best way to migrate this off the old API", true},
		{"audit the entire codebase for unused exports", true},
		{"design a plugin system for the tool registry", true},
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			score, reasons := routeScore(tt.msg)
			if got := score >= routeEscalateAt; got != tt.escalate {
				t.Errorf("score %d (%v), escalate = %v, want %v", score, reasons, got, tt.escalate)
			}
		})
	}
}

func TestShouldOfferEscalation(t *testing.T) {
	const planning = "how should I structure the router?"

	t.Run("offers when plan is bound elsewhere", func(t *testing.T) {
		m := routedModel(map[string]string{"plan": "big"}, "small")
		if ok, _ := m.shouldOfferEscalation(planning); !ok {
			t.Error("expected an offer")
		}
	})
	t.Run("silent when routing is unconfigured", func(t *testing.T) {
		m := routedModel(nil, "small")
		if ok, _ := m.shouldOfferEscalation(planning); ok {
			t.Error("offered with nothing to escalate to")
		}
	})
	t.Run("silent when already on the plan model", func(t *testing.T) {
		m := routedModel(map[string]string{"plan": "small"}, "small")
		if ok, _ := m.shouldOfferEscalation(planning); ok {
			t.Error("offered a swap to the model already loaded")
		}
	})
	t.Run("silent in plan and auto mode", func(t *testing.T) {
		for _, mode := range []Mode{PlanMode, AutoMode} {
			m := routedModel(map[string]string{"plan": "big"}, "small")
			m.mode = mode
			if ok, _ := m.shouldOfferEscalation(planning); ok {
				t.Errorf("offered in %s mode", mode)
			}
		}
	})
	t.Run("stops nagging after repeated declines", func(t *testing.T) {
		m := routedModel(map[string]string{"plan": "big"}, "small")
		m.routeDeclines = routeMaxDeclines
		if ok, _ := m.shouldOfferEscalation(planning); ok {
			t.Error("kept offering after being overruled")
		}
	})
}

// Routing off must not touch the active model on a mode switch.
func TestApplyRouteNoopWhenUnconfigured(t *testing.T) {
	m := routedModel(nil, "small")
	m.applyModeTransition(PlanMode, "")
	if m.modelName != "small" {
		t.Errorf("model = %q, want small (unconfigured routing changed the model)", m.modelName)
	}
}

// Provider-prefixed specs must swap the endpoint, not just the model name.
func TestRouteToProvider(t *testing.T) {
	m := routedModel(map[string]string{"plan": "openrouter:anthropic/claude-sonnet-4"}, "small")
	m.cfg.Providers = map[string]providerConfig{
		"openrouter": {BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OR_KEY"},
	}
	t.Setenv("OR_KEY", "sk-test")

	if !m.applyModeTransition(PlanMode, "") {
		t.Fatal("transition to plan was refused")
	}
	if m.modelName != "anthropic/claude-sonnet-4" {
		t.Errorf("model = %q, want the bare name with the provider prefix stripped", m.modelName)
	}
	if m.host.URL() != "https://openrouter.ai/api/v1" {
		t.Errorf("host = %q, want the provider base url", m.host.URL())
	}
	if !m.host.IsOpenAI() {
		t.Error("provider host must speak the OpenAI wire format")
	}
	// No /api/show there, so the profile falls back instead of probing.
	if m.contextLimit != maxContextBudget {
		t.Errorf("contextLimit = %d, want the openai fallback %d", m.contextLimit, maxContextBudget)
	}

	// Leaving the routed mode has to return to the local daemon, not just the
	// local model name.
	if !m.applyModeTransition(WriteMode, "") {
		t.Fatal("transition to write was refused")
	}
	if m.modelName != "small" || m.host.URL() != "http://localhost:11434" {
		t.Errorf("fell back to %q on %q, want small on the default host", m.modelName, m.host.URL())
	}
	if m.host.IsOpenAI() {
		t.Error("default host must stay native ollama")
	}
}

// A colon is both the provider separator and part of ordinary model tags, so the
// prefix only counts when it names a configured provider.
func TestSplitRouteSpec(t *testing.T) {
	m := routedModel(nil, "small")
	m.cfg.Providers = map[string]providerConfig{"lmstudio": {BaseURL: "http://localhost:1234/v1"}}

	tests := []struct{ spec, provider, model string }{
		{"qwen3-coder:30b", "", "qwen3-coder:30b"},
		{"lmstudio:qwen3-coder:30b", "lmstudio", "qwen3-coder:30b"},
		{"gpt-oss:120b-cloud", "", "gpt-oss:120b-cloud"},
		{"anthropic/claude-sonnet-4", "", "anthropic/claude-sonnet-4"},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			p, mo := m.splitRouteSpec(tt.spec)
			if p != tt.provider || mo != tt.model {
				t.Errorf("got (%q, %q), want (%q, %q)", p, mo, tt.provider, tt.model)
			}
		})
	}
}

// The key must be readable from the environment so it never has to sit in
// config.json in plaintext.
func TestProviderKeyPrefersEnv(t *testing.T) {
	t.Setenv("MY_KEY", "from-env")
	p := providerConfig{APIKey: "from-config", APIKeyEnv: "MY_KEY"}
	if got := providerKey(p); got != "from-env" {
		t.Errorf("providerKey = %q, want from-env", got)
	}
	if got := providerKey(providerConfig{APIKeyEnv: "UNSET_KEY", APIKey: "fallback"}); got != "fallback" {
		t.Errorf("providerKey = %q, want the config fallback when the env var is unset", got)
	}
}

// The connection modal edits whichever endpoint is selected; saving must land in
// that endpoint's config, not always the default host.
func TestSettingsTargetRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	m := routedModel(nil, "small")
	m.cfg.Providers = map[string]providerConfig{
		"openrouter": {BaseURL: "https://openrouter.ai/api/v1"},
	}
	m.urlInput = textinput.New()
	m.keyInput = textinput.New()

	// Index 0 is always the default host, then providers alphabetically.
	if got := m.settingsTargets(); len(got) != 2 || got[0] != "" || got[1] != "openrouter" {
		t.Fatalf("targets = %q, want [default, openrouter]", got)
	}

	m.openSettings("openrouter")
	if m.state != stateSettings {
		t.Fatal("openSettings did not show the modal")
	}
	if m.urlInput.Value() != "https://openrouter.ai/api/v1" {
		t.Errorf("url field = %q, want the provider's base url", m.urlInput.Value())
	}

	m.keyInput.SetValue("sk-or-new")
	host := m.saveSettingsInputs()

	if got := m.cfg.Providers["openrouter"].APIKey; got != "sk-or-new" {
		t.Errorf("provider key = %q, want sk-or-new", got)
	}
	if m.cfg.APIKey != "" {
		t.Error("editing a provider must not touch the default host's key")
	}
	if !host.IsOpenAI() || host.URL() != "https://openrouter.ai/api/v1" {
		t.Errorf("probe host = %q, want the endpoint just edited", host.URL())
	}

	// Unknown provider: no modal, no crash.
	m.state = stateChat
	m.openSettings("nope")
	if m.state == stateSettings {
		t.Error("opened the modal for a provider that does not exist")
	}
}

// A key typed into a field that an env var silently overrides is the confusing
// failure this hint exists to prevent.
func TestSettingsKeyHintNamesTheOverride(t *testing.T) {
	m := routedModel(nil, "small")
	m.cfg.Providers = map[string]providerConfig{
		"withenv": {BaseURL: "u", APIKeyEnv: "PROVIDER_KEY"},
		"plain":   {BaseURL: "u"},
	}

	t.Setenv("PROVIDER_KEY", "set")
	if got := m.settingsKeyHint("withenv"); !strings.Contains(got, "overrides") {
		t.Errorf("hint = %q, want it to say the env var wins", got)
	}
	t.Setenv("PROVIDER_KEY", "")
	if got := m.settingsKeyHint("withenv"); !strings.Contains(got, "unset") {
		t.Errorf("hint = %q, want it to say the env var is unset", got)
	}
	if got := m.settingsKeyHint("plain"); !strings.Contains(got, "config.json") {
		t.Errorf("hint = %q, want it to say where an unprotected key is stored", got)
	}
}

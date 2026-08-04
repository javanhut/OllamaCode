package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"

	"github.com/javanhut/ollama_code/api"
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

// settingsModel is a fixture with the connection modal's inputs constructed.
func settingsModel(t *testing.T) *Model {
	t.Helper()
	m := routedModel(nil, "small")
	m.urlInput = textinput.New()
	m.keyInput = textinput.New()
	m.nameInput = textinput.New()
	m.envInput = textinput.New()
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
func TestSettingsEditsSelectedEndpoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	m := settingsModel(t)
	m.cfg.Providers = map[string]providerConfig{"openrouter": {BaseURL: "https://openrouter.ai/api/v1"}}

	// Default host first, providers alphabetically, new-provider slot last.
	if got := m.settingsTargets(); len(got) != 3 {
		t.Fatalf("targets = %d, want default + openrouter + new", len(got))
	}

	m.openSettings("openrouter")
	if m.state != stateSettings {
		t.Fatal("openSettings did not show the modal")
	}
	if m.urlInput.Value() != "https://openrouter.ai/api/v1" || m.nameInput.Value() != "openrouter" {
		t.Errorf("fields not loaded: name=%q url=%q", m.nameInput.Value(), m.urlInput.Value())
	}

	m.keyInput.SetValue("sk-or-new")
	host, err := m.saveSettingsInputs()
	if err != nil {
		t.Fatalf("save failed: %v", err)
	}
	if got := m.cfg.Providers["openrouter"].APIKey; got != "sk-or-new" {
		t.Errorf("provider key = %q, want sk-or-new", got)
	}
	if m.cfg.APIKey != "" {
		t.Error("editing a provider must not touch the default host's key")
	}
	if !host.IsOpenAI() || host.URL() != "https://openrouter.ai/api/v1" {
		t.Errorf("probe host = %q, want the endpoint just edited", host.URL())
	}

	m.state = stateChat
	m.openSettings("nope")
	if m.state == stateSettings {
		t.Error("opened the modal for a provider that does not exist")
	}
}

// Creating a provider from the blank slot, including the wire-format toggle.
func TestSettingsCreatesProvider(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	m := settingsModel(t)
	m.openSettings(newProviderTarget)
	if !m.settingsIsNew() {
		t.Fatal("did not land on the new-provider slot")
	}
	if len(m.settingsFields()) != 5 {
		t.Errorf("got %d fields, want name/url/key/env/wire", len(m.settingsFields()))
	}

	m.nameInput.SetValue("lmstudio")
	m.urlInput.SetValue("http://localhost:1234/v1")
	m.envInput.SetValue("LMS_KEY")
	m.settingsKind = api.ProviderOllama

	host, err := m.saveSettingsInputs()
	if err != nil {
		t.Fatalf("save failed: %v", err)
	}
	p, ok := m.cfg.Providers["lmstudio"]
	if !ok {
		t.Fatal("provider was not created")
	}
	if p.BaseURL != "http://localhost:1234/v1" || p.APIKeyEnv != "LMS_KEY" || p.Kind != api.ProviderOllama {
		t.Errorf("saved %+v, want the edited values including the wire toggle", p)
	}
	if host.IsOpenAI() {
		t.Error("wire toggle did not reach the built client")
	}
	// The cursor must follow the row that was just created, not stay on "new".
	if m.settingsTargetName() != "lmstudio" {
		t.Errorf("cursor on %q, want lmstudio", m.settingsTargetName())
	}
}

func TestSettingsRejectsBadProviderNames(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	m := settingsModel(t)
	m.cfg.Providers = map[string]providerConfig{"taken": {BaseURL: "u"}}

	tests := []struct{ label, name, url string }{
		{"empty", "", "http://x/v1"},
		{"colon breaks the route separator", "a:b", "http://x/v1"},
		{"space breaks route parsing", "a b", "http://x/v1"},
		{"duplicate", "taken", "http://x/v1"},
		{"no url", "fine", ""},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			m.openSettings(newProviderTarget)
			m.nameInput.SetValue(tt.name)
			m.urlInput.SetValue(tt.url)
			if _, err := m.saveSettingsInputs(); err == nil {
				t.Error("expected an error")
			}
		})
	}
	if len(m.cfg.Providers) != 1 {
		t.Errorf("rejected input still created providers: %v", m.cfg.Providers)
	}
}

// A rename that leaves routes pointing at the old name strands them: the spec
// parses as a plain model and silently runs on the default host.
func TestRenameAndDeleteRepointRoutes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	m := settingsModel(t)
	m.cfg.Providers = map[string]providerConfig{"old": {BaseURL: "http://x/v1"}}
	m.cfg.Routes = map[string]string{"plan": "old:big-model", "write": "small"}

	m.openSettings("old")
	m.nameInput.SetValue("new")
	if _, err := m.saveSettingsInputs(); err != nil {
		t.Fatalf("rename failed: %v", err)
	}
	if _, gone := m.cfg.Providers["old"]; gone {
		t.Error("old provider name survived the rename")
	}
	if got := m.cfg.Routes["plan"]; got != "new:big-model" {
		t.Errorf("route = %q, want it repointed to new:big-model", got)
	}
	if got := m.cfg.Routes["write"]; got != "small" {
		t.Errorf("unrelated route was rewritten to %q", got)
	}

	m.deleteSettingsProvider()
	if _, still := m.cfg.Routes["plan"]; still {
		t.Error("route bound to a deleted provider was left dangling")
	}
	if got := m.cfg.Routes["write"]; got != "small" {
		t.Errorf("unrelated route dropped on delete: %q", got)
	}
}

// A key typed into a field that an env var silently overrides is the confusing
// failure this hint exists to prevent.
func TestSettingsKeyHintNamesTheOverride(t *testing.T) {
	m := settingsModel(t)
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

// The plan is the only thing that survives into write mode, so the model must
// not be able to leave plan mode without recording one.
func TestPlanGate(t *testing.T) {
	t.Run("empty notes are blocked", func(t *testing.T) {
		m := routedModel(nil, "small")
		m.applyModeTransition(PlanMode, "")
		if !m.planGateBlocks(WriteMode) {
			t.Error("switch to write allowed with no plan")
		}
	})

	t.Run("notes left from an earlier task are blocked", func(t *testing.T) {
		m := routedModel(nil, "small")
		m.notes.set("plan for some unrelated task from before")
		m.applyModeTransition(PlanMode, "") // marks the pre-existing notes
		if !m.planGateBlocks(WriteMode) {
			t.Error("stale notes satisfied the gate")
		}
	})

	t.Run("a plan written this session passes", func(t *testing.T) {
		m := routedModel(nil, "small")
		m.applyModeTransition(PlanMode, "")
		m.notes.set("1. edit tui/route.go\n2. add a test")
		if m.planGateBlocks(WriteMode) {
			t.Fatal("a freshly written plan was blocked")
		}

		m.applyModeTransition(WriteMode, "plan approved")
		last := m.history[len(m.history)-1].Content
		if !strings.Contains(last, "Plan Summary from Session Notes") {
			t.Errorf("plan was not handed off; last message = %q", last)
		}
	})

	t.Run("only plan to write is gated", func(t *testing.T) {
		m := routedModel(nil, "small")
		m.applyModeTransition(PlanMode, "")
		if m.planGateBlocks(ExploreMode) {
			t.Error("retreating to explore was blocked; it hands nothing off")
		}
		m.applyModeTransition(ExploreMode, "")
		if m.planGateBlocks(WriteMode) {
			t.Error("gated a switch that did not start in plan mode")
		}
	})

	t.Run("message names the model that will execute", func(t *testing.T) {
		m := routedModel(map[string]string{"plan": "big", "write": "small"}, "small")
		m.applyModeTransition(PlanMode, "")
		msg := m.planGateMessage()
		if !strings.Contains(msg, "update_session_notes") {
			t.Errorf("message = %q, want the action to take", msg)
		}
		if !strings.Contains(msg, "small") {
			t.Errorf("message = %q, want the executing model named", msg)
		}
	})
}

// A cursor provider drives a local CLI, so it has no URL to require and its
// profile must advertise no tools — it speaks no tool protocol.
func TestCursorProviderRouting(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	m := settingsModel(t)
	m.openSettings(newProviderTarget)
	m.nameInput.SetValue("cursor")
	m.settingsKind = api.ProviderCursor
	// URL left blank on purpose: that means "find cursor-agent on PATH".
	if _, err := m.saveSettingsInputs(); err != nil {
		t.Fatalf("a cursor provider must not require a URL: %v", err)
	}

	m.cfg.Routes = map[string]string{"plan": "cursor:claude-sonnet-4"}
	m.applyModeTransition(PlanMode, "")

	if m.modelName != "claude-sonnet-4" {
		t.Errorf("model = %q, want the bare model name", m.modelName)
	}
	if !m.host.IsCursor() {
		t.Fatal("host does not drive the cursor CLI")
	}
	if m.profile.SupportsTools {
		t.Error("tools offered to an agent CLI — it would echo fake tool JSON as prose")
	}
	if !strings.HasPrefix(m.activeSystemPrompt(), agentProviderPrompt) {
		t.Error("agent provider got the full tool-protocol prompt")
	}
}

// The whole point of offloading: cursor plans, and Layla picks it up without the
// user touching anything. The planner has no tools, so it can neither record the
// plan nor call switch_mode — both have to happen for it.
func TestOffloadedPlanHandsOffToLocalModel(t *testing.T) {
	m := routedModel(map[string]string{"plan": "big", "write": "small"}, "small")
	m.applyModeTransition(PlanMode, "")
	m.profile = ModelProfile{SupportsTools: false} // an agent CLI provider

	if !m.planGateBlocks(WriteMode) {
		t.Fatal("setup: gate should start closed")
	}

	plan := "1. edit tui/route.go\n2. add a test"
	if !m.handOffOffloadedPlan(plan) {
		t.Fatal("handoff refused")
	}

	if m.notes.get() != plan {
		t.Error("plan was not recorded; write mode receives nothing")
	}
	if m.mode != WriteMode {
		t.Errorf("mode = %s, want write — the turn would dead-end waiting for shift+tab", m.mode)
	}
	if m.modelName != "small" {
		t.Errorf("model = %q, want the local model executing", m.modelName)
	}

	var summary, handoff bool
	for _, msg := range m.history {
		if strings.Contains(msg.Content, "Plan Summary from Session Notes") {
			summary = true
		}
		if strings.Contains(msg.Content, "[PLAN HANDOFF]") {
			handoff = true
		}
	}
	if !summary {
		t.Error("the plan was not injected into history")
	}
	if !handoff {
		t.Error("no handoff directive — nothing tells the executor to verify the plan")
	}
}

func TestOffloadedHandoffIsNarrow(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*Model)
		answer string
	}{
		{"tool-capable model writes its own notes", func(m *Model) {
			m.profile = ModelProfile{SupportsTools: true}
		}, "a plan"},
		{"empty answer", func(m *Model) {
			m.profile = ModelProfile{SupportsTools: false}
		}, "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := routedModel(nil, "small")
			m.applyModeTransition(PlanMode, "")
			tt.setup(m)
			if m.handOffOffloadedPlan(tt.answer) {
				t.Error("handed off when it should not have")
			}
			if m.mode != PlanMode {
				t.Errorf("mode = %s, want plan (unchanged)", m.mode)
			}
			if m.notes.get() != "" {
				t.Errorf("notes clobbered with %q", m.notes.get())
			}
		})
	}

	t.Run("only from plan mode", func(t *testing.T) {
		m := routedModel(nil, "small")
		m.profile = ModelProfile{SupportsTools: false}
		if m.handOffOffloadedPlan("a plan") {
			t.Error("handed off from explore mode")
		}
	})
}

// workspace makes a temp dir the process cwd with the given files in it, so the
// plan's existence checks resolve the same way they do against a real repo.
func workspace(t *testing.T, files ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		full := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
}

func TestCheckPlanFindsPaths(t *testing.T) {
	workspace(t, "tui/route.go")

	plan := "1. Edit `tui/route.go` to add a field\n2. Update tui/nope_not_real.go\n3. See https://example.com/a/b for context"
	c := checkPlan(plan)

	if !c.actionable() {
		t.Fatal("a plan naming files was judged unactionable")
	}
	if !slices.Contains(c.named, "tui/route.go") {
		t.Errorf("named = %v, want tui/route.go (backticks stripped)", c.named)
	}
	if !slices.Contains(c.missing, "tui/nope_not_real.go") {
		t.Errorf("missing = %v, want the nonexistent file flagged", c.missing)
	}
	if slices.Contains(c.missing, "tui/route.go") {
		t.Error("an existing file was reported missing")
	}
	for _, p := range c.named {
		if strings.Contains(p, "example.com") {
			t.Errorf("a URL was treated as a workspace path: %q", p)
		}
	}
	// An absolute or escaping path in a plan must never be stat'd or handed on.
	for _, p := range checkPlan("edit /etc/passwd and ../../outside.go").named {
		t.Errorf("path outside the workspace accepted: %q", p)
	}
}

// A plan that names no file is a question or a refusal, and must not reach write
// mode — there is nothing to execute and the local model would improvise.
func TestNonPlanDoesNotHandOff(t *testing.T) {
	for _, answer := range []string{
		"Which framework are you using? I need to know before I can plan this.",
		"I cannot help with that request.",
	} {
		m := routedModel(nil, "small")
		m.applyModeTransition(PlanMode, "")
		m.profile = ModelProfile{SupportsTools: false}

		if m.handOffOffloadedPlan(answer) {
			t.Errorf("handed off a non-plan: %q", answer)
		}
		if m.mode != PlanMode {
			t.Errorf("mode = %s, want plan so the user's reply goes back to the planner", m.mode)
		}
	}
}

// The enforced half: a file the plan named cannot be edited before it is read.
func TestRequireReadBeforeEdit(t *testing.T) {
	arm := func(t *testing.T) *Model {
		t.Helper()
		workspace(t, "tui/route.go", "tui/mode.go")
		m := routedModel(nil, "small")
		m.planNeedsVerify = true
		m.planPaths = map[string]bool{"tui/route.go": true}
		m.turnReads = map[string]int{}
		return m
	}

	t.Run("unread file is refused", func(t *testing.T) {
		m := arm(t)
		reason := m.requireReadBeforeEdit("edit_file", []string{"tui/route.go"})
		if reason == "" {
			t.Fatal("edit allowed before the file was read")
		}
		if !strings.Contains(reason, "tui/route.go") {
			t.Errorf("reason = %q, want it to name the file", reason)
		}
	})

	t.Run("allowed once read", func(t *testing.T) {
		m := arm(t)
		m.turnReads["read_file\x01tui/route.go"] = 1
		if reason := m.requireReadBeforeEdit("edit_file", []string{"tui/route.go"}); reason != "" {
			t.Errorf("refused after the file was read: %s", reason)
		}
	})

	t.Run("a new file cannot be read first", func(t *testing.T) {
		m := arm(t)
		m.planPaths["tui/brand_new.go"] = true
		if reason := m.requireReadBeforeEdit("write_file", []string{"tui/brand_new.go"}); reason != "" {
			t.Errorf("blocked creation of a file that does not exist yet: %s", reason)
		}
	})

	t.Run("files outside the plan are not gated", func(t *testing.T) {
		m := arm(t)
		if reason := m.requireReadBeforeEdit("edit_file", []string{"tui/mode.go"}); reason != "" {
			t.Errorf("gated a file the plan never named: %s", reason)
		}
	})

	t.Run("inactive on an ordinary turn", func(t *testing.T) {
		m := arm(t)
		m.planNeedsVerify = false
		if reason := m.requireReadBeforeEdit("edit_file", []string{"tui/route.go"}); reason != "" {
			t.Errorf("gate active outside a plan handoff: %s", reason)
		}
	})
}

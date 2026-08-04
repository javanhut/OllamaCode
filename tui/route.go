package tui

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
)

// Mode→model routing. The mode state machine is already the task classifier:
// plan is where the expensive thinking happens, write is where the mechanical
// edits happen. Binding a model per mode routes the hard part to the big model
// without a separate classifier to get wrong.
//
// Routing re-points m.modelName itself rather than only the chat call, so
// everything derived from it — the profile, num_ctx, the small-model tier and
// its lean toolset, sub-agents, format repair — follows the routed model for
// free. Doing it at the call site instead would leave sub-agents on the
// previous model.

// modelForMode resolves the route spec a mode should run on: its binding, else
// the configured default. A spec is "<model>" on the default host, or
// "<provider>:<model>" for a configured endpoint. Empty means routing isn't
// configured — callers leave the active model alone, so an unconfigured install
// behaves exactly as before.
func (m *Model) modelForMode(mode Mode) string {
	if len(m.cfg.Routes) == 0 {
		return ""
	}
	if n := strings.TrimSpace(m.cfg.Routes[mode.String()]); n != "" {
		return n
	}
	// An unbound mode falls back to the default, so leaving a bound mode doesn't
	// strand the expensive model on cheap work.
	return strings.TrimSpace(m.cfg.Model)
}

// splitRouteSpec separates the provider prefix from the model name. Model names
// contain colons themselves ("qwen3-coder:30b"), so a prefix only counts when it
// names a provider that is actually configured — which also means a provider must
// not be named after a model family.
func (m *Model) splitRouteSpec(spec string) (provider, model string) {
	if name, rest, ok := strings.Cut(spec, ":"); ok {
		if _, known := m.cfg.Providers[name]; known {
			return name, rest
		}
	}
	return "", spec
}

// defaultHost builds a client for the configured Ollama daemon.
func (m *Model) defaultHost() api.OllamaHost {
	h := api.OllamaHost{}
	h.SetURI(m.cfg.Host)
	h.SetAPIKey(resolveAPIKey(m.cfg))
	return h
}

// hostForSpec builds the client a route spec runs against, plus the bare model
// name to send it.
func (m *Model) hostForSpec(spec string) (api.OllamaHost, string) {
	provider, model := m.splitRouteSpec(spec)
	if provider == "" {
		return m.defaultHost(), model
	}
	return m.providerHost(provider), model
}

// providerHost builds a client for a named provider.
func (m *Model) providerHost(name string) api.OllamaHost {
	p := m.cfg.Providers[name]
	h := api.OllamaHost{}
	h.SetURI(p.BaseURL)
	h.SetAPIKey(providerKey(p))
	kind := p.Kind
	if kind == "" {
		kind = api.ProviderOpenAI
	}
	h.SetProvider(kind)
	return h
}

// providerKey prefers the environment variable, so an API key never has to be
// written to config.json in plaintext.
func providerKey(p providerConfig) string {
	if p.APIKeyEnv != "" {
		if k := strings.TrimSpace(os.Getenv(p.APIKeyEnv)); k != "" {
			return k
		}
	}
	return strings.TrimSpace(p.APIKey)
}

// reloadActiveHost rebuilds the live client from config without changing which
// model is loaded. Needed after an endpoint's URL or key is edited: applyRoute
// would see the same model on the same URL and skip, leaving the old key in use.
func (m *Model) reloadActiveHost() {
	spec := m.modelForMode(m.mode)
	if spec == "" {
		spec = m.modelName // routing unconfigured: default host, current model
	}
	m.host, _ = m.hostForSpec(spec)
}

// settingsTargets lists the endpoints the connection modal can edit: the default
// host first, each provider alphabetically, then a trailing slot for a new one.
// Both the default host and the new slot have an empty name, so they are told
// apart by position — first and last, never the same entry.
func (m *Model) settingsTargets() []string {
	out := append([]string{""}, slices.Sorted(maps.Keys(m.cfg.Providers))...)
	return append(out, "")
}

// settingsIsNew reports whether the modal is on the "add a provider" slot.
func (m *Model) settingsIsNew() bool {
	return m.settingsTarget == len(m.settingsTargets())-1
}

// settingsTargetName is the provider being edited, or "" for the default host
// and for a provider not created yet.
func (m *Model) settingsTargetName() string {
	targets := m.settingsTargets()
	if m.settingsIsNew() || m.settingsTarget < 0 || m.settingsTarget >= len(targets) {
		return ""
	}
	return targets[m.settingsTarget]
}

// settingsIsProvider reports whether the selected endpoint is a provider —
// existing or about to be created — rather than the default host.
func (m *Model) settingsIsProvider() bool {
	return m.settingsIsNew() || m.settingsTargetName() != ""
}

// settingsFields lists the focusable rows for the selected endpoint. The default
// host has no name, env var, or wire format to set.
func (m *Model) settingsFields() []settingsField {
	if !m.settingsIsProvider() {
		return []settingsField{settingsFocusURL, settingsFocusKey}
	}
	return []settingsField{settingsFocusName, settingsFocusURL, settingsFocusKey, settingsFocusEnv, settingsFocusNative}
}

// loadSettingsInputs fills the modal's fields from the selected endpoint.
func (m *Model) loadSettingsInputs() {
	set := func(name, url, key, env, kind string) {
		m.nameInput.SetValue(name)
		m.urlInput.SetValue(url)
		m.keyInput.SetValue(key)
		m.envInput.SetValue(env)
		m.settingsKind = kind
	}
	switch {
	case m.settingsIsNew():
		set("", "", "", "", api.ProviderOpenAI)
	case m.settingsTargetName() != "":
		p := m.cfg.Providers[m.settingsTargetName()]
		set(m.settingsTargetName(), p.BaseURL, p.APIKey, p.APIKeyEnv, p.Kind)
	default:
		set("", m.cfg.Host, m.cfg.APIKey, "", api.ProviderOpenAI)
	}
	m.focusSettingsField(m.settingsFields()[0])
}

// validProviderName rejects names that would break route specs: the colon
// separates provider from model, and /route splits its arguments on whitespace.
func validProviderName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("name is required")
	case strings.ContainsAny(name, ": \t"):
		return fmt.Errorf("name cannot contain ':' or spaces")
	}
	return nil
}

// renameProvider moves a provider and repoints any routes at it. Without the
// route fixup a rename strands specs like "old:model", which then parse as a
// plain model name and silently run on the default host.
func (m *Model) renameProvider(old, updated string) {
	if old == "" || old == updated {
		return
	}
	m.cfg.Providers[updated] = m.cfg.Providers[old]
	delete(m.cfg.Providers, old)
	for mode, spec := range m.cfg.Routes {
		if prov, model, ok := strings.Cut(spec, ":"); ok && prov == old {
			m.cfg.Routes[mode] = updated + ":" + model
		}
	}
}

// clearRoutesFor drops any route bound to a provider that no longer exists, so a
// deleted endpoint fails loudly at bind time instead of silently falling through
// to the default host.
func (m *Model) clearRoutesFor(provider string) {
	for mode, spec := range m.cfg.Routes {
		if prov, _, ok := strings.Cut(spec, ":"); ok && prov == provider {
			delete(m.cfg.Routes, mode)
		}
	}
}

// saveSettingsInputs persists the edited endpoint and returns a client for it, so
// the caller can verify the key actually works against the one just changed.
func (m *Model) saveSettingsInputs() (api.OllamaHost, error) {
	uri := strings.TrimSpace(m.urlInput.Value())
	key := strings.TrimSpace(m.keyInput.Value())

	if !m.settingsIsProvider() {
		if uri == "" {
			uri = DefaultHost
		}
		m.cfg.Host = uri
		m.cfg.APIKey = key
		saveConfig(m.cfg)
		m.reloadActiveHost()
		return m.defaultHost(), nil
	}

	name := strings.TrimSpace(m.nameInput.Value())
	if err := validProviderName(name); err != nil {
		return api.OllamaHost{}, err
	}
	// A cursor provider has no URL to require: a blank command means "find
	// the Cursor agent on PATH", which is what almost everyone wants.
	if uri == "" && m.settingsKind != api.ProviderCursor {
		return api.OllamaHost{}, fmt.Errorf("base URL is required")
	}
	old := m.settingsTargetName()
	if name != old {
		if _, taken := m.cfg.Providers[name]; taken {
			return api.OllamaHost{}, fmt.Errorf("provider %q already exists", name)
		}
	}

	if m.cfg.Providers == nil {
		m.cfg.Providers = map[string]providerConfig{}
	}
	m.renameProvider(old, name)
	m.cfg.Providers[name] = providerConfig{
		BaseURL:   uri,
		APIKey:    key,
		APIKeyEnv: strings.TrimSpace(m.envInput.Value()),
		Kind:      m.settingsKind,
	}
	saveConfig(m.cfg)

	// The list just changed shape; keep the cursor on what was edited.
	if i := slices.Index(m.settingsTargets(), name); i >= 0 {
		m.settingsTarget = i
	}
	m.reloadActiveHost()
	return m.providerHost(name), nil
}

// openSettings shows the connection modal. An empty name selects the default
// host; newProviderTarget starts a blank provider.
const newProviderTarget = "new"

func (m *Model) openSettings(name string) {
	switch {
	case name == "":
		m.settingsTarget = 0
	case name == newProviderTarget:
		m.settingsTarget = len(m.settingsTargets()) - 1
	default:
		i := slices.Index(m.settingsTargets(), name)
		if i < 0 {
			m.toast = "no such provider: " + name + " — /provider to list them"
			return
		}
		m.settingsTarget = i
	}
	m.state = stateSettings
	m.statusMsg = ""
	m.statusErr = false
	m.loadSettingsInputs()
}

// routeIsLoaded reports whether a spec resolves to what is already running.
func (m *Model) routeIsLoaded(spec string) bool {
	host, name := m.hostForSpec(spec)
	return name == m.modelName && host.URL() == m.host.URL()
}

// applyRoute points the active model — and its endpoint — at whatever the mode
// resolves to, then reloads the profile. The reload is the point: num_ctx, tool
// support and the small-model cutoff are per-model, and assembling a 128k prompt
// for an 8k model is the failure this exists to prevent. Reports whether it
// swapped.
func (m *Model) applyRoute(mode Mode) bool {
	spec := m.modelForMode(mode)
	if spec == "" || m.routeIsLoaded(spec) {
		return false
	}
	m.host, m.modelName = m.hostForSpec(spec)
	m.resolveProfile()
	return true
}

// restoreDefaultModel drops back to the configured default. Needed when the last
// route is removed: applyRoute deliberately no-ops once routing is unconfigured,
// so without this the model would stay stranded on the route just deleted.
func (m *Model) restoreDefaultModel() {
	d := strings.TrimSpace(m.cfg.Model)
	if d == "" || m.routeIsLoaded(d) {
		return
	}
	m.host, m.modelName = m.hostForSpec(d)
	m.resolveProfile()
}

// routeNote describes the model swap a pending switch_mode would cause, for the
// permission modal — so accepting is an informed choice about which model (and
// whose tokens) runs next, not just about the mode.
func (m *Model) routeNote(target Mode) string {
	spec := m.modelForMode(target)
	if spec == "" || m.routeIsLoaded(spec) {
		return ""
	}
	return fmt.Sprintf("\nModel: %s → %s", m.modelName, spec)
}

// Cold start. The model escalates to plan mode itself via switch_mode once it
// is running — but on the first message of a task it hasn't run yet, and a small
// local model is exactly the one least likely to notice it is out of its depth.
// This scores the user's message and, above a threshold, offers the swap.
//
// It only ever ASKS. The user's y/N is the real classifier, so the scoring can
// be crude without being wrong — a false positive costs one keystroke.

// routeSignals are phrases that mark a request as planning-shaped, with coarse
// weights. Matched as substrings on the lowercased message.
var routeSignals = []struct {
	phrase string
	weight int
}{
	// Design questions — planning-shaped on their own.
	{"how should", 2}, {"best way", 2}, {"best approach", 2}, {"design", 2},
	{"architect", 2}, {"trade-off", 2}, {"tradeoff", 2}, {"strategy", 2},
	{"figure out", 2}, {"come up with", 2}, {"pros and cons", 2}, {"options for", 2},

	// Big verbs — these can still describe a one-line change, so they need
	// something else to corroborate before they reach the threshold.
	{"refactor", 1}, {"migrate", 1}, {"rewrite", 1}, {"restructure", 1},
	{"redesign", 1}, {"overhaul", 1}, {"audit", 1}, {"investigate", 1},
	{"reorganize", 1}, {"port ", 1},

	// Breadth — what turns a small edit into something worth planning.
	{"across", 1}, {"entire", 1}, {"whole", 1}, {"all the", 1}, {"everywhere", 1},
	{"codebase", 1}, {"end to end", 1}, {"end-to-end", 1}, {"throughout", 1}, {"every ", 1},
}

// routeEscalateAt is the score at or above which we offer the big model. Two
// means one design phrase, or a big verb plus corroborating breadth/scale —
// "refactor this function" scores 1 and stays local.
const routeEscalateAt = 2

// routeLongMessage is the word count past which a message reads as a spec
// rather than an instruction.
const routeLongMessage = 60

// routeScore rates how planning-shaped a message is, returning the score and the
// human-readable signals behind it. The reasons are shown in the prompt: a
// router the user can't audit is one they'll learn to dismiss unread.
func routeScore(msg string) (int, []string) {
	lower := strings.ToLower(msg)
	score := 0
	var reasons []string
	for _, s := range routeSignals {
		if strings.Contains(lower, s.phrase) {
			score += s.weight
			reasons = append(reasons, strings.TrimSpace(s.phrase))
		}
	}
	if n := strings.Count(msg, "@"); n >= 3 {
		score++
		reasons = append(reasons, fmt.Sprintf("%d file references", n))
	}
	if n := len(strings.Fields(msg)); n > routeLongMessage {
		score++
		reasons = append(reasons, fmt.Sprintf("%d words", n))
	}
	return score, reasons
}

// routeMaxDeclines stops the offer after the user has said no this many times.
// A router that keeps asking after being overruled is worse than no router.
const routeMaxDeclines = 2

// shouldOfferEscalation reports whether to interrupt this turn with the offer,
// and the signals that triggered it.
func (m *Model) shouldOfferEscalation(msg string) (bool, []string) {
	switch {
	case m.mode == PlanMode || m.mode == AutoMode:
		return false, nil // already planning, or explicitly autonomous — don't interrupt
	case m.routeDeclines >= routeMaxDeclines:
		return false, nil
	case m.modelForMode(PlanMode) == "" || m.routeIsLoaded(m.modelForMode(PlanMode)):
		return false, nil // nothing to escalate to
	}
	score, reasons := routeScore(msg)
	return score >= routeEscalateAt, reasons
}

// updateRouteConfirm answers the escalation offer. Either way the turn proceeds
// with the message the user already typed — the only question was which model
// runs it, so declining must never cost them the turn.
func (m *Model) updateRouteConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		m.state = stateChat
		m.input.Focus()
		m.applyModeTransition(PlanMode, "routed: planning task")
	case "n", "esc":
		m.state = stateChat
		m.input.Focus()
		m.routeDeclines++
		m.toast = "staying local on " + m.modelName
		if m.routeDeclines >= routeMaxDeclines {
			m.toast += " — routing offers off for this session (/route to change)"
		}
	default:
		return m, nil
	}
	return m, tea.Batch(m.resumeRoutedTurn()...)
}

// resumeRoutedTurn releases the message held back for the offer.
func (m *Model) resumeRoutedTurn() []tea.Cmd {
	ask := m.routeAsk
	m.routeAsk, m.routeReasons = "", nil
	cmds := m.startStreamWithRAGGate(ask)
	m.refreshTranscript()
	m.viewport.GotoBottom()
	return cmds
}

const routeUsage = "usage: /route <explore|plan|write|auto> <model> · /route <mode> off · /route off — no args shows the table"

// routeCommand implements /route: bind a model to a mode, unbind one, or clear
// routing entirely.
func (m *Model) routeCommand(args string) {
	fields := strings.Fields(args)
	switch len(fields) {
	case 0:
		m.showRoutes()
		return
	case 1:
		if !strings.EqualFold(fields[0], "off") && !strings.EqualFold(fields[0], "clear") {
			m.toast = routeUsage
			return
		}
		m.cfg.Routes = nil
		saveConfig(m.cfg)
		m.restoreDefaultModel()
		m.toast = "routing off — all modes use " + m.cfg.Model
		return
	case 2:
	default:
		m.toast = routeUsage
		return
	}

	mode, ok := parseMode(fields[0])
	if !ok {
		m.toast = "invalid mode: " + fields[0] + " (choose explore, plan, write, auto)"
		return
	}
	name := fields[1]

	if strings.EqualFold(name, "off") {
		delete(m.cfg.Routes, mode.String())
		saveConfig(m.cfg)
		m.applyRoute(m.mode)
		if len(m.cfg.Routes) == 0 {
			m.restoreDefaultModel()
		}
		m.toast = mode.String() + " unbound"
		return
	}

	if m.cfg.Routes == nil {
		m.cfg.Routes = map[string]string{}
	}
	m.cfg.Routes[mode.String()] = name
	saveConfig(m.cfg)
	m.applyRoute(m.mode)

	m.toast = fmt.Sprintf("%s → %s", mode, name)
	// A cloud model without a key fails at stream time with an opaque status
	// code; say it here, while the user is looking at the thing they just typed.
	if strings.HasSuffix(name, "-cloud") && resolveAPIKey(m.cfg) == "" {
		m.toast += " — no API key set, add one in /settings"
	}
}

func (m *Model) showRoutes() {
	var b strings.Builder
	b.WriteString("Model routing (per mode):\n")
	for _, mode := range []Mode{ExploreMode, PlanMode, WriteMode, AutoMode} {
		mark := " "
		if mode == m.mode {
			mark = "*"
		}
		spec := strings.TrimSpace(m.cfg.Routes[mode.String()])
		if spec == "" {
			fmt.Fprintf(&b, "%s %-8s %s  (default)\n", mark, mode, m.cfg.Model)
			continue
		}
		provider, model := m.splitRouteSpec(spec)
		if provider == "" {
			fmt.Fprintf(&b, "%s %-8s %s\n", mark, mode, model)
			continue
		}
		fmt.Fprintf(&b, "%s %-8s %s  via %s (%s)\n", mark, mode, model, provider, m.cfg.Providers[provider].BaseURL)
	}
	b.WriteString("\nBind: /route plan <model> · unbind: /route plan off · clear all: /route off\n")
	b.WriteString("Switching mode swaps the model and reloads its context window, so plan can think on a big model while write executes on a small local one.\n")
	b.WriteString("To route to another endpoint, add it with /provider, then bind \"<provider>:<model>\".")
	m.history = append(m.history, api.Message{Role: "system", Content: b.String()})
	m.refreshTranscript()
	m.viewport.GotoBottom()
}

const providerUsage = "usage: /provider new · /provider <name> to edit · /provider remove <name> — no args lists them"

// providerCommand implements /provider. Adding and editing happen in the
// connection modal, not on this line: an API key typed here would be visible on
// screen and recallable from input history.
func (m *Model) providerCommand(args string) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		m.showProviders()
		return
	}

	switch fields[0] {
	case "new", "add":
		m.openSettings(newProviderTarget)

	case "key", "edit":
		if len(fields) != 2 {
			m.toast = providerUsage
			return
		}
		m.openSettings(fields[1])

	case "remove", "rm", "delete":
		if len(fields) != 2 {
			m.toast = providerUsage
			return
		}
		if _, ok := m.cfg.Providers[fields[1]]; !ok {
			m.toast = "no such provider: " + fields[1]
			return
		}
		delete(m.cfg.Providers, fields[1])
		// Drop routes bound to it too, so a deleted endpoint fails at bind time
		// instead of silently falling through to the default host.
		m.clearRoutesFor(fields[1])
		saveConfig(m.cfg)
		m.reloadActiveHost()
		m.toast = "removed provider " + fields[1]

	default:
		// A bare name is shorthand for editing that provider.
		if len(fields) == 1 {
			m.openSettings(fields[0])
			return
		}
		m.toast = providerUsage
	}
}

func (m *Model) showProviders() {
	var b strings.Builder
	if len(m.cfg.Providers) == 0 {
		b.WriteString("No extra providers configured. The default host is " + m.cfg.Host + ".\n\n")
	} else {
		b.WriteString("Providers:\n")
		for _, name := range slices.Sorted(maps.Keys(m.cfg.Providers)) {
			p := m.cfg.Providers[name]
			kind := providerKindLabel(p.Kind)
			key := "no key"
			if p.APIKeyEnv != "" {
				key = "$" + p.APIKeyEnv
				if providerKey(p) == "" {
					key += " (unset)"
				}
			} else if p.APIKey != "" {
				key = "key in config"
			}
			fmt.Fprintf(&b, "  %-12s %s  [%s, %s]\n", name, p.BaseURL, kind, key)
		}
		b.WriteString("\n")
	}
	b.WriteString(providerUsage + "\n")
	b.WriteString("Everything is edited in one modal: name, URL, masked API key, env var, and wire format.\n")
	b.WriteString("Naming an env var there keeps the key out of config.json entirely.")
	m.history = append(m.history, api.Message{Role: "system", Content: b.String()})
	m.refreshTranscript()
	m.viewport.GotoBottom()
}

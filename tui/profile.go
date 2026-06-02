package tui

// maxContextBudget caps how much context we ask Ollama to allocate, even if the
// model reports a larger window — keeps memory/latency sane on local hardware.
const maxContextBudget = 131072

// defaultContextLimit is the fallback when /api/show reports nothing usable.
const defaultContextLimit = 124000

// resolveProfile loads the cached profile for the current model, or discovers it
// via /api/show (context length + tool support) and caches it. It also applies
// the resulting num_ctx to m.contextLimit. Safe to call whenever the model
// changes; it degrades gracefully if the host is unreachable.
func (m *Model) resolveProfile() {
	name := m.modelName
	if name == "" {
		return
	}
	if m.cfg.Profiles != nil {
		if p, ok := m.cfg.Profiles[name]; ok && p.NumCtx > 0 {
			m.applyProfile(p)
			return
		}
	}

	p := ModelProfile{NumCtx: defaultContextLimit, SupportsTools: true}
	if show, err := m.host.ShowModel(name); err == nil {
		if n := show.ContextLength(); n > 0 {
			p.NumCtx = n
		}
		// Only override the optimistic default when /api/show actually reports
		// capabilities; an empty list means "unknown", not "no tools".
		if len(show.Capabilities) > 0 {
			p.SupportsTools = show.SupportsTools()
		}
	}

	if m.cfg.Profiles == nil {
		m.cfg.Profiles = map[string]ModelProfile{}
	}
	m.cfg.Profiles[name] = p
	saveConfig(m.cfg)
	m.applyProfile(p)
}

func (m *Model) applyProfile(p ModelProfile) {
	m.profile = p
	limit := p.NumCtx
	if limit <= 0 {
		limit = defaultContextLimit
	}
	if limit > maxContextBudget {
		limit = maxContextBudget
	}
	m.contextLimit = limit
}

// chatOptions builds the Ollama Options map from the active profile.
func (m *Model) chatOptions() map[string]any {
	opts := map[string]any{"num_ctx": m.contextLimit}
	if m.profile.Temperature != nil {
		opts["temperature"] = *m.profile.Temperature
	}
	if m.profile.TopP != nil {
		opts["top_p"] = *m.profile.TopP
	}
	if m.profile.NumPredict != nil {
		opts["num_predict"] = *m.profile.NumPredict
	}
	return opts
}

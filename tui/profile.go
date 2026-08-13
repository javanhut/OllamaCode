package tui

// maxContextBudget caps how much context we ask Ollama to allocate, even if the
// model reports a larger window — keeps memory/latency sane on local hardware.
const maxContextBudget = 131072

// defaultContextLimit is both the conservative fallback and the automatic local
// allocation ceiling. A model may advertise 128K+, but reserving that entire KV
// cache makes ordinary 5-20K coding prompts dramatically slower. Users can
// still opt into a larger window with `/model ctx`.
const defaultContextLimit = 32768

// resolveProfile loads the cached profile for the current model, or discovers it
// via /api/show (context length + tool support) and caches it. It also applies
// the resulting num_ctx to m.contextLimit. Safe to call whenever the model
// changes; it degrades gracefully if the host is unreachable.
func (m *Model) resolveProfile() {
	name := m.modelName
	if name == "" {
		return
	}

	// cursor-agent is an agent, not a model: it reads the workspace itself and
	// speaks no tool protocol, so offering it OllamaCode's tools would only get
	// fake tool JSON back inside its prose. SupportsTools false makes startStream
	// send none.
	if m.host.IsCursor() {
		m.applyProfile(ModelProfile{NumCtx: maxContextBudget, SupportsTools: false})
		return
	}

	// An OpenAI-compatible endpoint has no /api/show, so context length and
	// capabilities aren't discoverable — probing would be a guaranteed-failing
	// round trip on every mode switch. Assume a large, tool-capable model, which
	// is what gets routed to one, and let /model ctx override. ParamsB stays 0,
	// so it is treated as a big model (full prompt, full toolset).
	if m.host.IsOpenAI() {
		p := ModelProfile{NumCtx: maxContextBudget, SupportsTools: true}
		if cached, ok := m.cfg.Profiles[name]; ok && cached.NumCtx > 0 {
			p = cached
		}
		m.applyProfile(p)
		return
	}

	if m.cfg.Profiles != nil {
		// ParamsB == 0 also re-probes profiles cached before tier detection
		// existed; one /api/show per model switch is cheap and self-heals.
		if p, ok := m.cfg.Profiles[name]; ok && p.NumCtx > 0 && p.ParamsB > 0 {
			if !p.NumCtxExplicit && p.NumCtx > defaultContextLimit {
				p.NumCtx = defaultContextLimit
				m.cfg.Profiles[name] = p
				saveConfig(m.cfg)
			}
			m.applyProfile(p)
			return
		}
	}

	p := ModelProfile{NumCtx: defaultContextLimit, SupportsTools: true}
	if show, err := m.host.ShowModel(name); err == nil {
		if n := show.ContextLength(); n > 0 {
			p.NumCtx = min(n, defaultContextLimit)
		}
		// Only override the optimistic default when /api/show actually reports
		// capabilities; an empty list means "unknown", not "no tools".
		if len(show.Capabilities) > 0 {
			p.SupportsTools = show.SupportsTools()
			p.SupportsThinking = show.SupportsThinking()
		}
		p.ParamsB = show.ParamsB()
	}
	if cached, ok := m.cfg.Profiles[name]; ok {
		p = preserveProfileOverrides(p, cached)
	}

	if m.cfg.Profiles == nil {
		m.cfg.Profiles = map[string]ModelProfile{}
	}
	m.cfg.Profiles[name] = p
	saveConfig(m.cfg)
	m.applyProfile(p)
}

func preserveProfileOverrides(discovered, configured ModelProfile) ModelProfile {
	if configured.NumCtxExplicit && configured.NumCtx > 0 {
		discovered.NumCtx = configured.NumCtx
		discovered.NumCtxExplicit = true
	}
	discovered.CapabilityTier = configured.CapabilityTier
	discovered.MaxVisibleTools = configured.MaxVisibleTools
	discovered.ProfileMaxSteps = configured.ProfileMaxSteps
	discovered.ParallelTools = configured.ParallelTools
	discovered.MaxParallelTools = configured.MaxParallelTools
	discovered.Delegation = configured.Delegation
	discovered.RAGTokens = configured.RAGTokens
	discovered.RAGTopK = configured.RAGTopK
	discovered.ActionTemperature = configured.ActionTemperature
	discovered.ProseTemperature = configured.ProseTemperature
	discovered.ReviewPass = configured.ReviewPass
	discovered.Temperature = configured.Temperature
	discovered.TopP = configured.TopP
	discovered.NumPredict = configured.NumPredict
	return discovered
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
func (m *Model) chatOptions(action bool) map[string]any {
	return m.chatOptionsForRequest(action, m.contextLimit)
}

func (m *Model) chatOptionsForRequest(action bool, contextLimit int) map[string]any {
	opts := map[string]any{"num_ctx": contextLimit}
	if m.profile.Temperature != nil {
		opts["temperature"] = *m.profile.Temperature
	} else if action && m.profile.ActionTemperature != nil {
		opts["temperature"] = *m.profile.ActionTemperature
	} else if !action && m.profile.ProseTemperature != nil {
		opts["temperature"] = *m.profile.ProseTemperature
	} else if m.profile.smallModel() {
		// Greedy decoding improves tool selection and argument stability. A
		// tool-less finalization pass may be slightly freer without risking calls.
		if action {
			opts["temperature"] = 0.0
		} else {
			opts["temperature"] = 0.2
		}
	}
	if m.profile.TopP != nil {
		opts["top_p"] = *m.profile.TopP
	}
	if m.numPredictOverride > 0 {
		// One-shot escalation after a dead (truncated, content-less) response;
		// set by the chatDoneMsg retry path and cleared in startStream.
		opts["num_predict"] = m.numPredictOverride
	} else if m.profile.NumPredict != nil {
		opts["num_predict"] = *m.profile.NumPredict
	} else if action {
		// A tool selection should be a compact call, not an unbounded essay. This
		// also gives continuously-streaming repetition a hard server-side stop.
		opts["num_predict"] = 1024
	}
	return opts
}

// deadRetryNumPredict picks the generation budget for the retry after a dead
// response: 4x the budget the truncated request used, capped at
// max(4096, configured num_predict) so an explicit profile setting still bounds
// the escalation.
func deadRetryNumPredict(normal int, configured *int) int {
	if normal <= 0 {
		normal = 1024
	}
	cap := 4096
	if configured != nil && *configured > cap {
		cap = *configured
	}
	return min(4*normal, cap)
}

func (m *Model) parallelToolsEnabled() bool {
	return m.profile.parallelToolCalls() && m.host.ProviderCapabilities().ParallelTools
}

func (m *Model) parallelToolLimit() int {
	if !m.parallelToolsEnabled() {
		return 1
	}
	return m.profile.maxParallelToolCalls()
}

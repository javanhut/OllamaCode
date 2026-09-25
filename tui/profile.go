package tui

import (
	"fmt"
	"maps"
)

// maxContextBudget caps how much context we ask Ollama to allocate, even if the
// model reports a larger window — keeps memory/latency sane on local hardware.
const maxContextBudget = 131072

// defaultContextLimit is the conservative fallback when /api/show reports
// nothing usable. Allocating 124K blindly made a transient introspection error
// reserve a huge KV cache on local hardware and dramatically slowed startup.
// Models that advertise a larger window still get it (up to maxContextBudget),
// and users can override this with `/model ctx`.
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
	key := m.profileKey()

	// cursor-agent is an agent, not a model: it reads the workspace itself and
	// speaks no tool protocol, so offering it OllamaCode's tools would only get
	// fake tool JSON back inside its prose. SupportsTools false makes startStream
	// send none.
	if m.host.IsCursor() {
		p := ModelProfile{NumCtx: maxContextBudget, SupportsTools: false}
		if cached, ok := m.cfg.Profiles[key]; ok && cached.NumCtx > 0 {
			p = cached
			p.SupportsTools = false
		}
		m.applyProfile(p)
		return
	}

	// An OpenAI-compatible endpoint has no /api/show, so context length and
	// capabilities aren't discoverable — probing would be a guaranteed-failing
	// round trip on every mode switch. Assume a large, tool-capable model, which
	// is what gets routed to one, and let /model ctx override. ParamsB stays 0,
	// so it is treated as a big model (full prompt, full toolset).
	if m.host.IsOpenAI() {
		p := ModelProfile{NumCtx: maxContextBudget, SupportsTools: true}
		if cached, ok := m.cfg.Profiles[key]; ok && cached.NumCtx > 0 {
			p = cached
		}
		m.applyProfile(p)
		return
	}

	if m.cfg.Profiles != nil {
		// ParamsB == 0 also re-probes profiles cached before tier detection
		// existed; one /api/show per model switch is cheap and self-heals.
		if p, ok := m.cfg.Profiles[key]; ok && p.NumCtx > 0 && p.ParamsB > 0 && p.SupportsVision != nil && p.ModelfileSampling != nil {
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
			p.SupportsThinking = show.SupportsThinking()
			vision := show.SupportsVision()
			p.SupportsVision = &vision
		}
		p.ParamsB = show.ParamsB()
		sets := show.SetsSampling()
		p.ModelfileSampling = &sets
	}
	if cached, ok := m.cfg.Profiles[key]; ok {
		p = preserveProfileOverrides(p, cached)
	}

	if m.cfg.Profiles == nil {
		m.cfg.Profiles = map[string]ModelProfile{}
	}
	m.cfg.Profiles[key] = p
	saveConfig(m.cfg)
	m.applyProfile(p)
}

func preserveProfileOverrides(discovered, configured ModelProfile) ModelProfile {
	if configured.NumCtx > 0 {
		discovered.NumCtx = configured.NumCtx
	}
	discovered.CapabilityTier = configured.CapabilityTier
	discovered.MaxVisibleTools = configured.MaxVisibleTools
	discovered.ProfileMaxSteps = configured.ProfileMaxSteps
	discovered.CompactThreshold = configured.CompactThreshold
	discovered.CompactTokens = configured.CompactTokens
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
	opts := map[string]any{"num_ctx": m.contextLimit}
	maps.Copy(opts, m.samplingOptions(action))
	if m.profile.TopP != nil {
		opts["top_p"] = *m.profile.TopP
	}
	if m.profile.NumPredict != nil {
		opts["num_predict"] = *m.profile.NumPredict
	}
	return opts
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

// syncContextCeiling lowers this model's num_ctx to the window the host has
// actually been able to allocate. The api layer already retried a failed
// request down to a size that loads; without adopting the result here the TUI
// would keep assembling prompts for a window the server cannot give it, and
// every turn would pay the search again. Persisted to the profile, so the next
// session starts at the size that fits this machine.
func (m *Model) syncContextCeiling() {
	n := m.host.ContextCeiling(m.modelName)
	if n <= 0 || n >= m.contextLimit {
		return
	}
	previous := m.contextLimit
	p := m.profile
	p.NumCtx = n
	m.saveProfile(p)
	m.logActivity(fmt.Sprintf("host could not allocate a %d-token context; using %d", previous, n))
	m.toast = fmt.Sprintf("context reduced to %d tokens — host ran out of memory", n)
}

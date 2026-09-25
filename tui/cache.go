package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
	tracepkg "github.com/javanhut/ollama_code/internal/trace"
)

// promptStats is what a finished response says about how its prompt was
// served: how much came from the KV cache, and whether the model had to load.
type promptStats struct {
	cached int
	load   time.Duration
}

func statsOf(c api.ChatResponse) promptStats {
	return promptStats{cached: c.PromptEvalCached, load: time.Duration(c.LoadDuration)}
}

// cacheMonitor tracks prompt-cache reuse and model reloads for the session.
// Everything ocode does to keep the system prefix byte-stable only pays off if
// the host actually reuses it; this is where that becomes visible.
type cacheMonitor struct {
	// reported is set once the host has sent a nonzero cached count. Servers
	// that predate the field omit it, and a zero from them means "unknown",
	// not "missed".
	reported bool

	lastPrompt, lastCached       int
	sessionPrompt, sessionCached int
	reloads                      int

	// warmKey identifies the load the last response came from (host, model,
	// num_ctx). A load on the same key is a reload; a new key is expected.
	warmKey    string
	lastAnswer time.Time

	// gpu is the last /api/ps reading for the loaded model.
	gpu *gpuReading
}

// gpuReading is how the host placed the loaded model.
type gpuReading struct {
	model      string
	size, vram int64
	ctx        int // context_length the host loaded
	requested  int // num_ctx ocode sent
}

// vramShare is the fraction of the model in GPU memory, 0..1.
func (g gpuReading) vramShare() float64 {
	if g.size <= 0 {
		return 1
	}
	return float64(g.vram) / float64(g.size)
}

const (
	// reloadThreshold separates a real load from the few milliseconds a warm
	// model still reports.
	reloadThreshold = 500 * time.Millisecond
	// reloadIdleGrace is how long the model may sit idle before a load is
	// the keep_alive expiring rather than something this session did. It sits
	// under the 30m keep_alive ocode sends.
	reloadIdleGrace = 25 * time.Minute
	// gpuShareWarn is the VRAM share under which the model is flagged: below
	// it enough layers run on the CPU to slow generation several times over.
	gpuShareWarn = 0.97
)

type gpuCheckMsg struct {
	key     string
	reading *gpuReading
	err     error
}

// observePromptStats records one finished response. It returns a /api/ps
// check for a new model or window and after a reload, the only times the
// model's placement can change.
func (m *Model) observePromptStats(promptEval int, st promptStats) tea.Cmd {
	c := &m.cache
	if st.cached > 0 {
		c.reported = true
	}
	if promptEval > 0 {
		c.lastPrompt, c.lastCached = promptEval, st.cached
		if c.reported {
			c.sessionPrompt += promptEval
			c.sessionCached += st.cached
		}
	}

	key := fmt.Sprintf("%s|%s|%d", m.host.URL(), m.modelName, m.contextLimit)
	loaded := st.load >= reloadThreshold
	reloaded := loaded && c.warmKey == key && time.Since(c.lastAnswer) < reloadIdleGrace
	fresh := c.warmKey != key
	c.warmKey, c.lastAnswer = key, time.Now()

	if reloaded {
		c.reloads++
		m.toast = fmt.Sprintf("model reloaded mid-session (%s) — prompt cache lost; another client or model may be evicting it", shortDuration(st.load))
		m.noteActivity("model reloaded mid-session: load " + shortDuration(st.load))
	}
	if m.trace != nil {
		_ = m.trace.Record(tracepkg.Event{Kind: "prompt_cache", Turn: m.turnGen, Model: m.modelName,
			Metadata: map[string]any{"prompt_tokens": promptEval, "cached_tokens": st.cached,
				"load_ms": st.load.Milliseconds(), "reloaded": reloaded}})
	}
	if fresh {
		c.gpu = nil // a reading for the previous model/window says nothing now
	}
	if fresh || reloaded {
		return m.gpuCheckCmd(key)
	}
	return nil
}

// gpuCheckCmd asks the host how it placed the current model.
func (m *Model) gpuCheckCmd(key string) tea.Cmd {
	if m.host.IsOpenAI() || m.host.IsCursor() || strings.Contains(m.host.URL(), "ollama.com") {
		return nil // no /api/ps, or someone else's hardware
	}
	host, model, requested := m.host, m.modelName, m.contextLimit
	return func() tea.Msg {
		rm, ok, err := host.RunningModel(model)
		if err != nil || !ok {
			return gpuCheckMsg{key: key, err: err}
		}
		return gpuCheckMsg{key: key, reading: &gpuReading{model: model, size: rm.Size, vram: rm.SizeVRAM,
			ctx: rm.ContextLength, requested: requested}}
	}
}

// applyGPUCheck stores a reading and warns when the model does not fit.
func (m *Model) applyGPUCheck(msg gpuCheckMsg) {
	if msg.reading == nil || msg.key != m.cache.warmKey {
		return // failed, or the model changed while the check was in flight
	}
	g := *msg.reading
	m.cache.gpu = &g
	if g.vramShare() < gpuShareWarn {
		m.toast = fmt.Sprintf("%s is only %.0f%% in GPU memory — the rest runs on CPU and generation will be much slower. Try a smaller context (/model ctx) or OLLAMA_KV_CACHE_TYPE=q8_0 on the server", g.model, g.vramShare()*100)
		m.noteActivity(fmt.Sprintf("model partly on CPU: %.0f%% in VRAM (%s of %s)", g.vramShare()*100, humanBytes(g.vram), humanBytes(g.size)))
	}
	if m.trace != nil {
		_ = m.trace.Record(tracepkg.Event{Kind: "gpu_placement", Turn: m.turnGen, Model: m.modelName,
			Metadata: map[string]any{"size": g.size, "size_vram": g.vram, "context_length": g.ctx, "num_ctx": g.requested}})
	}
}

// cacheStatsRows is the /stats view of the monitor, as label/value pairs.
func (c cacheMonitor) cacheStatsRows() [][2]string {
	var rows [][2]string
	switch {
	case c.reported && c.lastPrompt > 0:
		v := fmt.Sprintf("last %s", pct(c.lastCached, c.lastPrompt))
		if c.sessionPrompt > 0 {
			v += " · session " + pct(c.sessionCached, c.sessionPrompt)
		}
		rows = append(rows, [2]string{"prompt cache", v})
	case c.lastPrompt > 0:
		rows = append(rows, [2]string{"prompt cache", "not reported by this host"})
	}
	if c.reloads > 0 {
		rows = append(rows, [2]string{"reloads", fmt.Sprintf("%d mid-session", c.reloads)})
	}
	if g := c.gpu; g != nil {
		v := fmt.Sprintf("%.0f%% in VRAM (%s of %s)", g.vramShare()*100, humanBytes(g.vram), humanBytes(g.size))
		rows = append(rows, [2]string{"gpu", v})
		if g.ctx > 0 {
			ctx := fmt.Sprintf("%dk", g.ctx/1000)
			if g.requested > 0 && g.ctx != g.requested {
				ctx += fmt.Sprintf(" (ocode asked for %dk)", g.requested/1000)
			}
			rows = append(rows, [2]string{"loaded context", ctx})
		}
	}
	return rows
}

func pct(part, whole int) string {
	if whole <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", float64(part)*100/float64(whole))
}

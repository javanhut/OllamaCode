package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/javanhut/ollama_code/api"
)

func cacheTestModel() *Model {
	return &Model{host: api.OllamaHost{}, modelName: "qwen3.8", contextLimit: 65536}
}

func TestReloadFlaggedOnlyOnAWarmModel(t *testing.T) {
	m := cacheTestModel()

	// First response: loading is expected.
	if cmd := m.observePromptStats(1000, promptStats{load: 4 * time.Second}); cmd == nil {
		t.Fatal("the first load should schedule a GPU check")
	}
	if m.cache.reloads != 0 {
		t.Fatal("the first load was counted as a reload")
	}

	// Warm model, no load: nothing to report.
	if cmd := m.observePromptStats(1100, promptStats{cached: 1000, load: 3 * time.Millisecond}); cmd != nil {
		t.Fatal("a warm response scheduled a GPU check")
	}

	// Same model and window, loading again minutes later: a reload.
	if cmd := m.observePromptStats(1200, promptStats{load: 3 * time.Second}); cmd == nil {
		t.Fatal("a reload should re-check GPU placement")
	}
	if m.cache.reloads != 1 || !strings.Contains(m.toast, "reloaded") {
		t.Fatalf("reloads=%d toast=%q", m.cache.reloads, m.toast)
	}
}

func TestLoadAfterIdleIsNotAReload(t *testing.T) {
	m := cacheTestModel()
	m.observePromptStats(1000, promptStats{load: time.Second})
	m.cache.lastAnswer = time.Now().Add(-40 * time.Minute) // keep_alive expired

	m.observePromptStats(1000, promptStats{load: time.Second})

	if m.cache.reloads != 0 {
		t.Fatal("a keep_alive expiry was blamed as a mid-session reload")
	}
}

func TestModelSwitchIsNotAReload(t *testing.T) {
	m := cacheTestModel()
	m.observePromptStats(1000, promptStats{load: time.Second})
	m.cache.gpu = &gpuReading{model: "qwen3.8", size: 1, vram: 1}
	m.modelName = "gemma4"

	cmd := m.observePromptStats(1000, promptStats{load: time.Second})

	if m.cache.reloads != 0 || cmd == nil || m.cache.gpu != nil {
		t.Fatalf("reloads=%d check=%v staleGPU=%v", m.cache.reloads, cmd != nil, m.cache.gpu != nil)
	}
}

func TestCacheRatiosNeedAReportingHost(t *testing.T) {
	m := cacheTestModel()
	m.observePromptStats(1000, promptStats{})
	if rows := m.cache.cacheStatsRows(); len(rows) == 0 || rows[0][1] != "not reported by this host" {
		t.Fatalf("rows = %v", rows)
	}

	m.observePromptStats(1000, promptStats{cached: 900})
	m.observePromptStats(1000, promptStats{cached: 500})
	rows := m.cache.cacheStatsRows()
	if rows[0][1] != "last 50% · session 70%" {
		t.Fatalf("rows = %v", rows)
	}
}

func TestGPUCheckWarnsWhenModelSpillsToCPU(t *testing.T) {
	m := cacheTestModel()
	m.observePromptStats(1000, promptStats{load: time.Second})
	key := m.cache.warmKey

	m.applyGPUCheck(gpuCheckMsg{key: key, reading: &gpuReading{model: "qwen3.8", size: 20 << 30, vram: 15 << 30, ctx: 32768, requested: 65536}})

	if !strings.Contains(m.toast, "75% in GPU memory") {
		t.Fatalf("toast = %q", m.toast)
	}
	rows := m.cache.cacheStatsRows()
	joined := ""
	for _, r := range rows {
		joined += r[0] + "=" + r[1] + ";"
	}
	if !strings.Contains(joined, "gpu=75% in VRAM") || !strings.Contains(joined, "ocode asked for 65k") {
		t.Fatalf("rows = %s", joined)
	}
}

func TestGPUCheckIgnoresStaleReading(t *testing.T) {
	m := cacheTestModel()
	m.observePromptStats(1000, promptStats{load: time.Second})
	m.applyGPUCheck(gpuCheckMsg{key: "some|other|key", reading: &gpuReading{size: 10, vram: 1}})
	if m.cache.gpu != nil || m.toast != "" {
		t.Fatal("a reading for a previous model was applied")
	}
}

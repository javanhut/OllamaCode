package tui

import (
	"testing"
)

func TestTokenRatioKey(t *testing.T) {
	if got := tokenRatioKey("ollama:http://localhost:11434", "qwen3:8b"); got != "ollama:http://localhost:11434|qwen3:8b" {
		t.Fatalf("unexpected key: %q", got)
	}
	// Distinct providers for the same model must not share a ratio.
	if tokenRatioKey("a", "m") == tokenRatioKey("b", "m") {
		t.Fatal("provider must be part of the key")
	}
}

func TestSaveAndLoadTokenRatio(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	key := tokenRatioKey("ollama:http://localhost:11434", "qwen3:8b")
	saveTokenRatio(key, 3.75)
	ratios := loadTokenRatios()
	if ratios[key] != 3.75 {
		t.Fatalf("ratio did not round trip: %v", ratios)
	}
	// Saving a second model keeps the first.
	other := tokenRatioKey("openrouter", "claude")
	saveTokenRatio(other, 4.5)
	ratios = loadTokenRatios()
	if ratios[key] != 3.75 || ratios[other] != 4.5 {
		t.Fatalf("expected both ratios persisted, got %v", ratios)
	}
}

func TestEnsureMeasuredRatioLoadsPersistedValue(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetRatio(t)
	m := &Model{modelName: "qwen3:8b"}
	saveTokenRatio(tokenRatioKey(m.ratioProvider(), m.modelName), 3.2)
	m.ensureMeasuredRatio()
	if CharsPerToken() != 3.2 {
		t.Fatalf("expected persisted ratio 3.2, got %f", CharsPerToken())
	}
	// A model with no recorded ratio resets to the default heuristic.
	m.modelName = "never-calibrated"
	m.ensureMeasuredRatio()
	if CharsPerToken() != defaultCharsPerToken {
		t.Fatalf("uncalibrated model should use the default, got %f", CharsPerToken())
	}
}

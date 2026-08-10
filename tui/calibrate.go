package tui

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/internal/calibration"
)

type calibrationDoneMsg struct {
	result calibration.Result
	err    error
}

// ratioProvider names the provider the same way calibrateModelCmd does, so the
// persisted chars-per-token ratio is keyed identically on save and load.
func (m *Model) ratioProvider() string {
	provider := m.activeProvider()
	if provider == "" {
		provider = "ollama:" + m.host.URL()
	}
	return provider
}

func tokenRatioKey(provider, model string) string {
	return provider + "|" + model
}

func (m *Model) calibrateModelCmd() tea.Cmd {
	host, model := m.host, m.modelName
	provider := m.ratioProvider()
	return func() tea.Msg {
		runtime := "unknown"
		if version, err := host.GetOllamaVersion(); err == nil {
			runtime = version
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		result, err := calibration.Run(ctx, host, model, provider, runtime)
		if err == nil {
			if models, listErr := host.GetModelList(); listErr == nil {
				result.Digest = models.DigestFor(model)
			}
		}
		if err == nil {
			err = calibration.Save(result)
		}
		if err == nil && result.CharsPerToken > 0 {
			// Install the measured ratio immediately and persist it so later
			// sessions estimate tokens with it from the first render.
			markRatioKey(tokenRatioKey(provider, model))
			SetCharsPerToken(result.CharsPerToken)
			saveTokenRatio(tokenRatioKey(provider, model), result.CharsPerToken)
		}
		return calibrationDoneMsg{result: result, err: err}
	}
}

// ensureMeasuredRatio loads the calibrated chars-per-token ratio for the
// current provider+model at most once per model switch; afterwards it is a
// mutex and a string compare, cheap enough for the render path. Models that
// were never calibrated stay on the default heuristic.
func (m *Model) ensureMeasuredRatio() {
	key := tokenRatioKey(m.ratioProvider(), m.modelName)
	if ratioKeySeen(key) {
		return
	}
	markRatioKey(key)
	SetCharsPerToken(loadTokenRatios()[key]) // missing key → 0 → default
}

func (m *Model) applyCalibration() {
	result := m.lastCalibration
	provider := m.activeProvider()
	if provider == "" {
		provider = "ollama:" + m.host.URL()
	}
	if result == nil || result.Model != m.modelName || result.Provider != provider {
		m.toast = "no current recommendation — run /model calibrate first"
		return
	}
	profile := m.profile
	profile.CapabilityTier = result.Recommended
	m.saveProfile(profile)
	m.toast = fmt.Sprintf("applied %s tier to %s from calibration %.0f%%", result.Recommended, m.modelName, result.Score()*100)
}

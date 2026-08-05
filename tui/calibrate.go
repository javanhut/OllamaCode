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

func (m *Model) calibrateModelCmd() tea.Cmd {
	host, model := m.host, m.modelName
	provider := m.activeProvider()
	if provider == "" {
		provider = "ollama:" + host.URL()
	}
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
		return calibrationDoneMsg{result: result, err: err}
	}
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

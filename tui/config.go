package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ollama_code", "config.json")
}

func defaultTracePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "ollama_code-trace.jsonl")
	}
	return filepath.Join(dir, "ollama_code", "trace.jsonl")
}

// resolveAPIKey returns the Ollama API key to authenticate requests with. The
// OLLAMA_API_KEY environment variable (matching the ollama CLI convention)
// takes precedence over the saved config so it can be supplied without writing
// the secret to disk. An empty result means unauthenticated local access.
func resolveAPIKey(c config) string {
	if k := strings.TrimSpace(os.Getenv("OLLAMA_API_KEY")); k != "" {
		return k
	}
	return strings.TrimSpace(c.APIKey)
}

func loadConfig() config {
	var c config
	path := configPath()
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &c)
		}
	}
	if strings.TrimSpace(c.Host) == "" {
		c.Host = DefaultHost
	}
	return c
}

func saveConfig(c config) {
	path := configPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

func tokenRatiosPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ollama_code", "token_ratios.json")
}

// loadTokenRatios reads the calibrated chars-per-token ratios, keyed by
// provider|model. A missing or corrupt file yields an empty map — the
// estimators then stay on their default heuristic.
func loadTokenRatios() map[string]float64 {
	ratios := map[string]float64{}
	path := tokenRatiosPath()
	if path == "" {
		return ratios
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &ratios)
	}
	return ratios
}

// saveTokenRatio persists the ratio measured by /model calibrate for one
// provider|model, keeping the ratios recorded for other models.
func saveTokenRatio(key string, ratio float64) {
	path := tokenRatiosPath()
	if path == "" {
		return
	}
	ratios := loadTokenRatios()
	ratios[key] = ratio
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(ratios, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

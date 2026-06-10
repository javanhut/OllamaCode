// Package backend manages the lifecycle of the local inference servers
// OllamaCode talks to. It can auto-start the Ollama server, and it bootstraps,
// launches, and supervises the bundled MLX bridge (mlx_lm + mlx_vlm) via uv so
// the user never has to run anything in a separate terminal.
package backend

import (
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

//go:embed bridge/mlx_bridge.py
var bridgeScript []byte

// DefaultMLXPort is the loopback port the MLX bridge listens on.
const DefaultMLXPort = 11550

// DefaultMLXURI is the base URL of the MLX bridge.
const DefaultMLXURI = "http://127.0.0.1:11550"

// baseDir is ~/.ollama_code/mlx, where the bridge script and logs live.
func baseDir() string {
	return filepath.Join(os.Getenv("HOME"), ".ollama_code", "mlx")
}

// materializeBridge writes the embedded bridge script to disk (refreshing it
// when the bundled copy changes) and returns its path.
func materializeBridge() (string, error) {
	dir := baseDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "mlx_bridge.py")
	if existing, err := os.ReadFile(path); err == nil && bytesEqual(existing, bridgeScript) {
		return path, nil
	}
	if err := os.WriteFile(path, bridgeScript, 0o644); err != nil {
		return "", fmt.Errorf("write bridge script: %w", err)
	}
	return path, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// httpHealthy reports whether a GET on url returns 2xx within timeout.
func httpHealthy(url string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

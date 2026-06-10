package backend

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// OllamaRunning reports whether an Ollama server is answering at uri.
func OllamaRunning(uri string) bool {
	return httpHealthy(strings.TrimRight(uri, "/")+"/api/version", 2*time.Second)
}

// EnsureOllama makes sure an Ollama server is reachable at uri. If it isn't and
// the `ollama` binary is installed, it starts `ollama serve` in the background
// and waits up to timeout for it to come up. The spawned server is detached so
// it keeps running for the user's session.
func EnsureOllama(ctx context.Context, uri string, timeout time.Duration) error {
	if OllamaRunning(uri) {
		return nil
	}
	bin, err := exec.LookPath("ollama")
	if err != nil {
		return fmt.Errorf("ollama is not running and the `ollama` binary was not found on PATH")
	}

	cmd := exec.Command(bin, "serve")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ollama serve: %w", err)
	}
	go cmd.Wait() // reap; the server is intentionally left running

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if OllamaRunning(uri) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("ollama serve did not become ready within %s", timeout)
			}
		}
	}
}

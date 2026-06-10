package backend

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// MLXSupervisor bootstraps and runs the bundled MLX bridge as a child process.
// First start may take a while: uv provisions Python + mlx_lm/mlx_vlm before
// the server can come up, so callers should WaitHealthy with a generous timeout.
type MLXSupervisor struct {
	port int

	mu  sync.Mutex
	cmd *exec.Cmd
}

// NewMLXSupervisor creates a supervisor for the bridge on the given port (0 =
// DefaultMLXPort).
func NewMLXSupervisor(port int) *MLXSupervisor {
	if port == 0 {
		port = DefaultMLXPort
	}
	return &MLXSupervisor{port: port}
}

// URI is the base URL the MLXClient should target.
func (s *MLXSupervisor) URI() string {
	return fmt.Sprintf("http://127.0.0.1:%d", s.port)
}

// Running reports whether the bridge process is currently alive.
func (s *MLXSupervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil && s.cmd.Process != nil && (s.cmd.ProcessState == nil || !s.cmd.ProcessState.Exited())
}

// Healthy reports whether the bridge is currently answering /health.
func (s *MLXSupervisor) Healthy() bool {
	return httpHealthy(s.URI()+"/health", 2*time.Second)
}

// Start ensures uv is available, writes the bridge script, and launches it via
// `uv run`. It returns once the process has been spawned; use WaitHealthy to
// block until the server is actually accepting requests. If the bridge is
// already healthy (e.g. left running), Start is a no-op.
func (s *MLXSupervisor) Start(ctx context.Context) error {
	if s.Healthy() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil && s.cmd.ProcessState == nil {
		return nil // already starting
	}

	uv, err := EnsureUV(ctx)
	if err != nil {
		return err
	}
	script, err := materializeBridge()
	if err != nil {
		return err
	}

	logPath := filepath.Join(baseDir(), "bridge.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open bridge log: %w", err)
	}

	// `uv run <script>` provisions the PEP 723 dependencies declared in the
	// script and runs it in an isolated, cached environment.
	cmd := exec.Command(uv, "run", script, "--host", "127.0.0.1", "--port", fmt.Sprintf("%d", s.port))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own process group so Stop kills children too
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("launch MLX bridge: %w", err)
	}
	s.cmd = cmd

	// Reap the process when it exits and close the log handle.
	go func() {
		cmd.Wait()
		logFile.Close()
	}()
	return nil
}

// WaitHealthy polls the bridge until it answers /health, the timeout elapses,
// or the context is cancelled. First-run dependency provisioning is slow, hence
// the long default timeout chosen by callers.
func (s *MLXSupervisor) WaitHealthy(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s.Healthy() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("MLX bridge did not become healthy within %s (see %s)",
					timeout, filepath.Join(baseDir(), "bridge.log"))
			}
		}
	}
}

// Stop terminates the bridge process group.
func (s *MLXSupervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	pid := s.cmd.Process.Pid
	// Kill the whole process group (uv -> python).
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	_ = s.cmd.Process.Kill()
	s.cmd = nil
}

package mcp

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func runShellCommand(ctx context.Context, command, workingDir, stdin string, timeout time.Duration) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.Command("sh", "-c", command)
	configureShellCommand(cmd)
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var out lockedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		return "", err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return shellCommandResult(out.String(), err)
	case <-cctx.Done():
		killShellCommand(cmd)
		err := <-done
		text := strings.TrimRight(out.String(), "\n")
		if cctx.Err() == context.DeadlineExceeded {
			if text == "" {
				return "[timed out after " + timeout.String() + "]", nil
			}
			return text + "\n[timed out after " + timeout.String() + "]", nil
		}
		return shellCommandResult(out.String(), err)
	}
}

func shellCommandResult(raw string, err error) (string, error) {
	text := strings.TrimRight(raw, "\n")
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return fmt.Sprintf("%s\n[exit %d]", text, exitErr.ExitCode()), nil
		}
		return "", err
	}
	if text == "" {
		return "[ok]", nil
	}
	return text, nil
}

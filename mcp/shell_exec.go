package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
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

	// Real pipe fd (an *os.File) instead of an io.Writer: os/exec hands the fd
	// straight to the child and starts NO internal copy goroutine, so cmd.Wait()
	// blocks only on the process — never on a stdout fd a lingering grandchild
	// (daemon, `&` job, gpg-agent, dev server) still holds open.
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return "", err
	}
	pw.Close() // parent drops its write end; only descendants keep it now

	var out lockedBuffer
	copyDone := make(chan struct{})
	go func() {
		io.Copy(&out, pr)
		close(copyDone)
	}()

	// Once the process is gone, its own output is already in the pipe; give the
	// copier a brief grace to drain, then a read deadline unblocks it even if a
	// grandchild still holds the write end open.
	drain := func() {
		_ = pr.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		<-copyDone
		pr.Close()
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		drain()
		return shellCommandResult(out.String(), err)
	case <-cctx.Done():
		killShellCommand(cmd)
		err := <-done
		drain()
		text := strings.TrimRight(out.String(), "\n")
		if cctx.Err() == context.DeadlineExceeded {
			msg := "[timed out after " + timeout.String() + "]"
			hint := "\n(killed — if this command is meant to keep running, re-run it with background=true)"
			if text == "" {
				return msg + hint, nil
			}
			return text + "\n" + msg + hint, nil
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

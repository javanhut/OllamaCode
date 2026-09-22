package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
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

// secretEnvFragments mark a variable name as a credential. Substring and
// case-insensitive, because the prefix is never predictable (OLLAMA_API_KEY,
// GH_TOKEN, aws_secret_access_key, PGPASSWORD). AUTH is deliberately absent: it
// would take SSH_AUTH_SOCK with it, and every ssh-based `git push` with that.
var secretEnvFragments = []string{"KEY", "SECRET", "TOKEN", "PASSWORD", "PASSWD", "CREDENTIAL", "_PAT", "DSN"}

func isSecretEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, fragment := range secretEnvFragments {
		if strings.Contains(upper, fragment) {
			return true
		}
	}
	return false
}

// credentialURL matches a URL carrying userinfo with a password —
// postgres://app:s3cr3t@host/db. DATABASE_URL, REDIS_URL, MONGODB_URI and
// AMQP_URL are among the most common secret-bearing variables on a dev machine
// and none of them trips a name fragment, so the value shape is checked too.
// Narrow on purpose: a bare `scheme://host` or a userless URL is left alone.
var credentialURL = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*://[^/@\s:]+:[^/@\s]+@`)

func isSecretEnvEntry(name, value string) bool {
	return isSecretEnvName(name) || credentialURL.MatchString(value)
}

// scrubbedEnvironment is the parent environment minus anything whose NAME looks
// like a credential. The model chooses what run_shell executes and the output
// comes back verbatim into the transcript and its own context, so `env`, or any
// build script that dumps its environment, was a free read of every secret in
// the user's shell. Names, not values: matching values would mangle ordinary
// output and still miss the short ones.
//
// A denylist, unlike allowedEnvironment (external.go), which is an allowlist
// for MCP subprocesses — those name the credentials they need in env_allow
// deliberately, a model-authored shell command never does. Everything ordinary
// (PATH, HOME, LANG, TERM, TMPDIR…) stays, or commands break. Applied in
// newShellCommand, so every shell spawn gets it.
//
// The cost is real: a command that authenticates from an environment credential
// (gh, aws, curl -H "Bearer $API_KEY") now sees it unset and must use a
// config-file or keychain login instead. No escape hatch until someone hits it.
func scrubbedEnvironment() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if name, value, ok := strings.Cut(entry, "="); !ok || !isSecretEnvEntry(name, value) {
			out = append(out, entry)
		}
	}
	return out
}

func runShellCommand(ctx context.Context, command, workingDir, stdin string, timeout time.Duration) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := newShellCommand(command)
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
			code := exitErr.ExitCode()
			return "", &CommandFailure{Output: fmt.Sprintf("%s\n[exit %d]", text, code), ExitCode: code}
		}
		return "", err
	}
	if text == "" {
		return "[ok]", nil
	}
	return text, nil
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/javanhut/ollama_code/internal/jobs"
)

// bgJob is a detached shell command started with run_shell(background=true). Its
// output accumulates in a buffer; a watcher goroutine records the exit status.
// Jobs outlive the tool call and turn — they run until they exit or are killed
// via shell_output(kill=true). Identity lives in the unified job registry
// (internal/jobs): the job id IS the registry id, shared with sub-agent jobs.
type bgJob struct {
	id      int
	command string
	pid     int
	out     *lockedBuffer
	cmd     *exec.Cmd
	started time.Time
	rec     *jobs.Job

	mu       sync.Mutex
	done     bool
	exitCode int
	exitErr  string
}

var (
	bgMu sync.Mutex
	// bgNotifier, when set, is invoked exactly once by the job's watcher
	// goroutine when the job exits — normally, in error, or killed via
	// shell_output. The TUI registers it to push completion notifications;
	// headless runs leave it nil and keep polling shell_output.
	bgNotifier func(jobID int, exitCode int, err error, tail string)
)

// bgNotifyTailLines bounds how much of a job's output the completion
// notification carries; the full log stays available via shell_output.
const bgNotifyTailLines = 20

// SetBackgroundShellNotifier registers (or clears, with nil) the completion
// hook for background shell jobs. Package-global like the job registry: the
// process has one conversation surface.
func SetBackgroundShellNotifier(fn func(jobID int, exitCode int, err error, tail string)) {
	bgMu.Lock()
	bgNotifier = fn
	bgMu.Unlock()
}

// tailLines returns the last n lines of s, trailing newlines stripped.
func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// startBackgroundShell launches command detached from the caller's context, so
// it keeps running after the tool call returns. Returns immediately with a job
// handle; the command is reaped by a background goroutine.
func startBackgroundShell(command, workingDir, stdin string) (*bgJob, error) {
	cmd := newShellCommand(command)
	configureShellCommand(cmd) // own process group, so kill reaches children
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	// Real pipe fd (an *os.File): os/exec hands it straight to the child and
	// starts NO internal copy goroutine, so cmd.Wait() records the exit status as
	// soon as the direct process exits — even if a grandchild (worker, daemon)
	// keeps stdout open. Output keeps accumulating via our own reader until the
	// whole process tree closes the pipe (true EOF) or the job is killed.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	out := &lockedBuffer{}
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return nil, err
	}
	pw.Close() // parent drops its write end; only descendants keep it now

	go func() {
		io.Copy(out, pr)
		pr.Close()
	}()

	job := &bgJob{command: command, pid: cmd.Process.Pid, out: out, cmd: cmd, started: time.Now()}
	// Register into the unified registry: the returned id is the job id the
	// model sees, shared with sub-agent jobs. The hooks let the generic job
	// tools (and shell_output) read and kill the job without knowing about
	// bgJob.
	job.rec = jobs.Default().Register(jobs.KindShell, shortCommand(command), jobs.Hooks{
		Status: job.statusLine,
		Output: func() string { return job.out.String() },
		Cancel: func() { killShellCommand(job.cmd) },
	})
	job.id = job.rec.ID()

	go func() {
		err := cmd.Wait()
		job.mu.Lock()
		job.done = true
		if ee, ok := err.(*exec.ExitError); ok {
			job.exitCode = ee.ExitCode()
		} else if err != nil {
			job.exitErr = err.Error()
		}
		exitCode := job.exitCode
		exitErr := job.exitErr
		job.mu.Unlock()
		// Settle the registry record. A kill requested through the registry
		// rewrites this to killed; a signal death from elsewhere (ExitError
		// with code -1) reads as failed, like any non-zero exit.
		switch {
		case exitErr != "":
			job.rec.Fail(exitErr)
		case exitCode != 0:
			job.rec.Fail(fmt.Sprintf("exit %d", exitCode))
		default:
			job.rec.Finish("exit 0")
		}
		// The watcher runs once per job and a kill funnels through the same
		// Wait, so the hook fires exactly once on any exit path.
		bgMu.Lock()
		notify := bgNotifier
		bgMu.Unlock()
		if notify != nil {
			notify(job.id, exitCode, err, tailLines(job.out.String(), bgNotifyTailLines))
		}
	}()
	return job, nil
}

func (j *bgJob) statusLine() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.done {
		return fmt.Sprintf("running (pid %d, %s elapsed)", j.pid, time.Since(j.started).Round(time.Second))
	}
	if j.exitErr != "" {
		return "exited (" + j.exitErr + ")"
	}
	return fmt.Sprintf("exited %d", j.exitCode)
}

// shortCommand renders the first line of a command for one-line listings.
func shortCommand(s string) string {
	s = strings.TrimSpace(s)
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return strings.TrimSpace(before) + " …"
	}
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// ShellOutputTool reads/stops background shell jobs (run_shell background=true).
func ShellOutputTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "shell_output",
			Description: "Read the accumulated output and status of a background shell job started with run_shell(background=true). Pass kill=true to stop it (kills its whole process group). Omit the job id to list all background jobs.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"job":  {Type: "number", Description: "Background job id returned by run_shell. Omit to list all jobs."},
					"kill": {Type: "boolean", Description: "Stop the job before returning its output."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Job  int  `json:"job"`
				Kill bool `json:"kill"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			// shell_output is the model's long-standing habit; it now reads the
			// shell section of the unified job registry (see tools/jobs.go).
			if a.Job == 0 {
				return listJobsText(jobs.KindShell), nil
			}
			job, ok := jobs.Default().Get(a.Job)
			if !ok {
				return "", fmt.Errorf("no background job %d (use shell_output with no arguments to list jobs)", a.Job)
			}
			if job.Kind() != jobs.KindShell {
				return "", fmt.Errorf("job %d is a background %s job, not a shell job — read it with job_output", a.Job, job.Kind())
			}
			if a.Kill {
				if _, err := jobs.Default().Cancel(a.Job); err != nil {
					return "", err
				}
			}
			return renderJobOutput(job), nil
		},
	}
}

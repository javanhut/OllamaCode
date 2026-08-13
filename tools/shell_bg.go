package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// bgJob is a detached shell command started with run_shell(background=true). Its
// output accumulates in a buffer; a watcher goroutine records the exit status.
// Jobs outlive the tool call and turn — they run until they exit or are killed
// via shell_output(kill=true).
type bgJob struct {
	id      int
	command string
	pid     int
	out     *lockedBuffer
	cmd     *exec.Cmd
	started time.Time

	mu       sync.Mutex
	done     bool
	exitCode int
	exitErr  string
}

var (
	bgMu   sync.Mutex
	bgJobs = map[int]*bgJob{}
	bgNext = 1
)

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

	bgMu.Lock()
	id := bgNext
	bgNext++
	job := &bgJob{id: id, command: command, pid: cmd.Process.Pid, out: out, cmd: cmd, started: time.Now()}
	bgJobs[id] = job
	bgMu.Unlock()

	go func() {
		err := cmd.Wait()
		job.mu.Lock()
		job.done = true
		if ee, ok := err.(*exec.ExitError); ok {
			job.exitCode = ee.ExitCode()
		} else if err != nil {
			job.exitErr = err.Error()
		}
		job.mu.Unlock()
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

func lookupBgJob(id int) *bgJob {
	bgMu.Lock()
	defer bgMu.Unlock()
	return bgJobs[id]
}

// BackgroundJobCount returns the number of live (not yet exited) background
// shell jobs. The TUI's stagnation guard uses it to tell "polling a running
// job" apart from a no-progress loop.
func BackgroundJobCount() int {
	bgMu.Lock()
	jobs := make([]*bgJob, 0, len(bgJobs))
	for _, j := range bgJobs {
		jobs = append(jobs, j)
	}
	bgMu.Unlock()
	live := 0
	for _, j := range jobs {
		j.mu.Lock()
		if !j.done {
			live++
		}
		j.mu.Unlock()
	}
	return live
}

// bgJobOutputTail caps how much buffered output a BgJobInfo snapshot carries.
const bgJobOutputTail = 2048

// BgJobInfo is a point-in-time snapshot of a background shell job, for UIs
// (e.g. a /jobs command) that list and manage jobs. Output is the tail (last
// ~2KB) of the job's buffered combined stdout+stderr, not the full buffer.
// ExitCode is meaningful only when Done is true; it is -1 for a job killed by
// a signal.
type BgJobInfo struct {
	ID       int
	Command  string
	PID      int
	Started  time.Time
	Done     bool
	ExitCode int
	Output   string
}

// snapshotJobs returns every registered job, sorted by ID. Finished jobs are
// kept in the registry on purpose: the list should show them too.
func snapshotJobs() []*bgJob {
	bgMu.Lock()
	jobs := make([]*bgJob, 0, len(bgJobs))
	for _, j := range bgJobs {
		jobs = append(jobs, j)
	}
	bgMu.Unlock()
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].id < jobs[k].id })
	return jobs
}

// ListBgJobs returns a snapshot of every background job started this session,
// running or finished, sorted by job ID. Safe for concurrent use.
func ListBgJobs() []BgJobInfo {
	jobs := snapshotJobs()
	infos := make([]BgJobInfo, 0, len(jobs))
	for _, j := range jobs {
		out := j.out.String()
		if len(out) > bgJobOutputTail {
			out = out[len(out)-bgJobOutputTail:]
		}
		j.mu.Lock()
		infos = append(infos, BgJobInfo{
			ID:       j.id,
			Command:  j.command,
			PID:      j.pid,
			Started:  j.started,
			Done:     j.done,
			ExitCode: j.exitCode,
			Output:   out,
		})
		j.mu.Unlock()
	}
	return infos
}

// KillBgJob SIGKILLs the process group of the job with the given ID. Killing
// an already-finished job is a no-op (nil error) so callers never race the
// watcher that marks jobs done; an unknown ID is an error.
func KillBgJob(id int) error {
	job := lookupBgJob(id)
	if job == nil {
		return fmt.Errorf("no background job %d", id)
	}
	job.mu.Lock()
	done := job.done
	job.mu.Unlock()
	if done {
		return nil
	}
	killShellCommand(job.cmd)
	return nil
}

// KillAllBackgroundJobs SIGKILLs the process group of every live background
// job and marks it done without waiting for the reaper. It is the session-end
// cleanup (wired into TUI shutdown) so detached jobs — dev servers, watchers —
// never outlive the process. Safe to call multiple times; finished jobs and
// repeat calls are no-ops.
func KillAllBackgroundJobs() {
	for _, j := range snapshotJobs() {
		j.mu.Lock()
		if j.done {
			j.mu.Unlock()
			continue
		}
		// Mark done up front: a concurrent KillAllBackgroundJobs/KillBgJob
		// then skips the kill. The watcher goroutine re-marks after Wait and
		// records the real exit status — that write is idempotent.
		j.done = true
		j.mu.Unlock()
		killShellCommand(j.cmd)
	}
}

func listBgJobsText() string {
	bgMu.Lock()
	ids := make([]int, 0, len(bgJobs))
	for id := range bgJobs {
		ids = append(ids, id)
	}
	bgMu.Unlock()
	if len(ids) == 0 {
		return "no background jobs"
	}
	sort.Ints(ids)
	var b strings.Builder
	for _, id := range ids {
		j := lookupBgJob(id)
		fmt.Fprintf(&b, "job %d: %s — %s\n", id, j.statusLine(), shortCommand(j.command))
	}
	return strings.TrimRight(b.String(), "\n")
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
			if a.Job == 0 {
				return listBgJobsText(), nil
			}
			job := lookupBgJob(a.Job)
			if job == nil {
				return "", fmt.Errorf("no background job %d (use shell_output with no arguments to list jobs)", a.Job)
			}
			if a.Kill {
				killShellCommand(job.cmd)
			}
			out := strings.TrimRight(job.out.String(), "\n")
			if out == "" {
				return fmt.Sprintf("job %d: %s\n(no output yet)", a.Job, job.statusLine()), nil
			}
			return fmt.Sprintf("job %d: %s\n%s", a.Job, job.statusLine(), out), nil
		},
	}
}

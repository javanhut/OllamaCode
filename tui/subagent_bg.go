package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// This file is the background half of spawn_subagent (async=true, the
// default). It mirrors the bg-shell pattern from tools/shell_bg.go: the tool
// call returns immediately with a job handle, a detached goroutine does the
// work, and completion is surfaced to the user and the model. Where bg shell
// jobs are polled via shell_output, a sub-agent job has a single bounded final
// report, so the report itself is delivered in the completion notification —
// a system message appended to the conversation (see the subagentDoneMsg
// handler in update.go), which wakes the parent when it is idle.

// maxBackgroundSubagentJobs bounds concurrently RUNNING background jobs, so a
// model can't fork an unbounded fleet across turns (each job may itself fan
// out to maxParallelSubagents workers).
const maxBackgroundSubagentJobs = 4

// subagentJobTimeout caps one background job's wall-clock run, matching the
// budget the sync path gets from invokeToolCmd.
var subagentJobTimeout = longRunningToolTimeout

// subagentJob is one background spawn_subagent call (one or more tasks).
type subagentJob struct {
	id      int
	tasks   []string
	started time.Time
	cancel  context.CancelFunc

	mu      sync.Mutex
	done    bool
	mutated bool   // a file-mutating tool call was observed via the Before hook
	report  string // final combined report, set on completion
	// interrupted marks a job cancelled by the user's esc/ctrl+c interrupt;
	// its completion notification is recorded but does not auto-wake the parent.
	interrupted bool
}

func (j *subagentJob) finish(report string) {
	j.mu.Lock()
	j.done = true
	j.report = report
	j.mu.Unlock()
}

func (j *subagentJob) markMutated() {
	j.mu.Lock()
	j.mutated = true
	j.mu.Unlock()
}

// snapshot reads the completion state under the job lock.
func (j *subagentJob) snapshot() (done, mutated bool, report string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done, j.mutated, j.report
}

// wasInterrupted reports whether the job was cancelled by a user interrupt.
func (j *subagentJob) wasInterrupted() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.interrupted
}

// notification renders the completion message injected into the conversation
// when the job finishes. It carries the full reports, so the parent never has
// to poll for results.
func (j *subagentJob) notification() string {
	elapsed := time.Since(j.started).Round(time.Second)
	tasks := "1 task"
	if len(j.tasks) != 1 {
		tasks = fmt.Sprintf("%d tasks", len(j.tasks))
	}
	_, _, report := j.snapshot()
	if j.wasInterrupted() {
		return fmt.Sprintf("[SUB-AGENT JOB %d CANCELLED] (%s, %s) The background sub-agent job was stopped by the user. Partial report (if any):\n\n%s",
			j.id, tasks, elapsed, report)
	}
	return fmt.Sprintf("[SUB-AGENT JOB %d COMPLETE] (%s, %s) The background sub-agent(s) you spawned have finished. Their reports:\n\n%s",
		j.id, tasks, elapsed, report)
}

// statusLine renders one line of job status for toasts and the status surface.
func (j *subagentJob) statusLine() string {
	done, _, _ := j.snapshot()
	if done {
		return fmt.Sprintf("done (%s)", time.Since(j.started).Round(time.Second))
	}
	return fmt.Sprintf("running (%s elapsed)", time.Since(j.started).Round(time.Second))
}

// subagentStore tracks background jobs. All access is mutex-guarded: jobs are
// spawned from tool goroutines and completed on detached worker goroutines,
// while the update loop reads status.
type subagentStore struct {
	mu   sync.Mutex
	jobs map[int]*subagentJob
	next int
}

func newSubagentStore() *subagentStore {
	return &subagentStore{jobs: map[int]*subagentJob{}, next: 1}
}

// running returns the jobs still executing.
func (s *subagentStore) running() []*subagentJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*subagentJob
	for _, j := range s.jobs {
		if done, _, _ := j.snapshot(); !done {
			out = append(out, j)
		}
	}
	return out
}

// all returns every job, running or finished, sorted by id. Finished jobs are
// kept in the store on purpose: the /jobs modal shows them too, like the
// shell background-job registry does.
func (s *subagentStore) all() []*subagentJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*subagentJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].id < out[k].id })
	return out
}

// cancel stops one running job by id, marking it interrupted so its
// completion notification doesn't auto-wake the parent. Returns false when
// there is no such running job (unknown id or already finished).
func (s *subagentStore) cancel(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[id]
	if j == nil {
		return false
	}
	if done, _, _ := j.snapshot(); done {
		return false
	}
	if j.cancel != nil {
		j.mu.Lock()
		j.interrupted = true
		j.mu.Unlock()
		j.cancel()
	}
	return true
}

// cancelAll stops every running job (interrupt path). Jobs finish with a
// cancelled report and still deliver their completion notification, but the
// interrupted flag keeps that notification from auto-waking the parent.
func (s *subagentStore) cancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if done, _, _ := j.snapshot(); !done && j.cancel != nil {
			j.mu.Lock()
			j.interrupted = true
			j.mu.Unlock()
			j.cancel()
		}
	}
}

// subagentDoneMsg is delivered to the update loop when a background job
// finishes. The job carries its final report.
type subagentDoneMsg struct{ job *subagentJob }

// ensureSubagentRuntime lazily builds the job store and event channel, so a
// Model constructed outside NewModel (tests) can still spawn.
func (m *Model) ensureSubagentRuntime() {
	if m.subagents == nil {
		m.subagents = newSubagentStore()
	}
	if m.subagentEvents == nil {
		m.subagentEvents = make(chan *subagentJob, 64)
	}
}

// spawnSubagentAsync starts the tasks as a detached background job and returns
// the handle text immediately. The job's file mutations are banked into the
// parent's /undo checkpoint via the same Before hook the sync path uses — the
// store is mutex-guarded, so the hook is safe on the worker goroutine.
// Ordering caveat: if the parent turn ends before the job finishes, later
// snapshots land in whichever turn checkpoint is open when the write happens
// (a fresh pending bank if none is), so /undo attribution for those detached
// writes belongs to that later checkpoint, not the spawning turn.
func (m *Model) spawnSubagentAsync(tasks []string) (string, error) {
	m.ensureSubagentRuntime()
	if n := len(m.subagents.running()); n >= maxBackgroundSubagentJobs {
		return "", fmt.Errorf("%d background sub-agent jobs are still running (max %d) — wait for their completion notifications, or spawn with async=false to run inline", n, maxBackgroundSubagentJobs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), subagentJobTimeout)
	m.subagents.mu.Lock()
	id := m.subagents.next
	m.subagents.next++
	job := &subagentJob{id: id, tasks: tasks, started: time.Now(), cancel: cancel}
	m.subagents.jobs[id] = job
	m.subagents.mu.Unlock()

	opts := m.subagentOptions(m.checkpointBeforeCall(), job.markMutated)

	go func() {
		defer cancel()
		report, err := m.runSubagentTasks(ctx, tasks, opts)
		if err != nil {
			report = "(failed: " + err.Error() + ")"
		} else if ctx.Err() != nil {
			report = "(cancelled)\n\n" + report
		}
		job.finish(report)
		// Buffered (64) and jobs are capped, so this never blocks in practice;
		// the drop fallback keeps a pathological no-update-loop Model from
		// leaking the worker goroutine.
		select {
		case m.subagentEvents <- job:
		default:
		}
	}()

	label := tasks[0]
	if len(tasks) > 1 {
		label = fmt.Sprintf("%d tasks", len(tasks))
	}
	return fmt.Sprintf("Spawned background sub-agent job %d (%s). It is running now; you will receive its report(s) as a completion notification — do NOT respawn or wait idly, continue with other work or tell the user what is underway. Task: %s",
		id, label, truncatePlain(strings.Join(tasks, " | "), 120)), nil
}

// awaitSubagentEvent blocks until a background job completes and delivers it
// as a subagentDoneMsg. One waiter is armed from Init and re-armed by the
// subagentDoneMsg handler, so at most one goroutine parks on the channel.
func (m *Model) awaitSubagentEvent() tea.Cmd {
	events := m.subagentEvents
	if events == nil {
		return nil
	}
	return func() tea.Msg {
		job := <-events
		if job == nil {
			return nil
		}
		return subagentDoneMsg{job: job}
	}
}

// runningSubagentJobs reports how many background sub-agent jobs are still
// executing — surfaced in toasts and (via the parent) the status line.
func (m *Model) runningSubagentJobs() int {
	if m.subagents == nil {
		return 0
	}
	return len(m.subagents.running())
}

// cancelSubagents stops all running background sub-agent jobs. Called from the
// esc/ctrl+c interrupt paths in update.go.
func (m *Model) cancelSubagents() {
	if m.subagents != nil {
		m.subagents.cancelAll()
	}
}

package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/jobs"
	"github.com/javanhut/ollama_code/tools"
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
var subagentJobTimeout = tools.PolicyForName("spawn_subagent").Timeout

// subagentJob is one background spawn_subagent call (one or more tasks). Its
// id comes from the unified job registry (internal/jobs), shared with
// background shell jobs; rec is the registry record, settled by the worker
// goroutine so job_list/job_output/job_kill see live state. A single-task
// job's conversation is retained on completion (see subagentStore.retain) so
// the parent can resume the child with a follow-up (spawn_subagent
// resume_job); retention is in-memory only — nothing is persisted to disk.
type subagentJob struct {
	id      int
	tasks   []string
	started time.Time
	cancel  context.CancelFunc
	rec     *jobs.Job

	mu      sync.Mutex
	done    bool
	mutated bool   // a file-mutating tool call was observed via the Before hook
	report  string // final combined report, set on completion
	// history is the child's retained conversation (seed + exchanges + final
	// answer) for follow-ups; nil for parallel multi-task jobs and after
	// eviction by the retention cap.
	history []api.Message
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

// setHistory stores (or clears, on eviction) the retained conversation.
func (j *subagentJob) setHistory(history []api.Message) {
	j.mu.Lock()
	j.history = history
	j.mu.Unlock()
}

// historySnapshot reads the retained conversation under the job lock.
func (j *subagentJob) historySnapshot() []api.Message {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.history
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
	// Point the model at the resume path when the child's conversation is
	// still retained (a follow-up is cheaper than a fresh agent).
	hint := ""
	if len(j.historySnapshot()) > 0 {
		hint = fmt.Sprintf(" To ask this sub-agent a follow-up, call spawn_subagent with resume_job=%d.", j.id)
	}
	if j.wasInterrupted() {
		return fmt.Sprintf("[SUB-AGENT JOB %d CANCELLED] (%s, %s) The background sub-agent job was stopped by the user. Partial report (if any):\n\n%s%s",
			j.id, tasks, elapsed, report, hint)
	}
	return fmt.Sprintf("[SUB-AGENT JOB %d COMPLETE] (%s, %s) The background sub-agent(s) you spawned have finished. Their reports:\n\n%s%s",
		j.id, tasks, elapsed, report, hint)
}

// statusLine renders one line of job status for toasts and the status surface.
func (j *subagentJob) statusLine() string {
	done, _, _ := j.snapshot()
	if done {
		return fmt.Sprintf("done (%s)", time.Since(j.started).Round(time.Second))
	}
	return fmt.Sprintf("running (%s elapsed)", time.Since(j.started).Round(time.Second))
}

// outputSnapshot is the registry output hook: unlike a shell job, a sub-agent
// has no intermediate stream to tail, so this is the final report once done
// and a still-running note while live.
func (j *subagentJob) outputSnapshot() string {
	done, _, report := j.snapshot()
	if !done {
		return fmt.Sprintf("(still running, %s elapsed)", time.Since(j.started).Round(time.Second))
	}
	return report
}

// maxRetainedSubagentHistories bounds how many finished children's
// conversations are kept for follow-ups (memory only — no disk persistence).
// The oldest beyond the cap are evicted; resuming an evicted child fails with
// a clear error (see Model.resumeTarget).
const maxRetainedSubagentHistories = 8

// subagentStore tracks background jobs. All access is mutex-guarded: jobs are
// spawned from tool goroutines and completed on detached worker goroutines,
// while the update loop reads status. Ids are NOT minted here — they come
// from the unified job registry (internal/jobs), shared with shell jobs.
type subagentStore struct {
	mu   sync.Mutex
	jobs map[int]*subagentJob
	// retained lists job ids whose conversations are kept for follow-ups, in
	// retention order (oldest first), capped at maxRetainedSubagentHistories.
	retained []int
}

func newSubagentStore() *subagentStore {
	return &subagentStore{jobs: map[int]*subagentJob{}}
}

// retain stores the job's completed conversation for later follow-ups and
// evicts the oldest retained history beyond the cap. Re-retaining a job (a
// sync follow-up updating its history) moves it to the newest slot.
func (s *subagentStore) retain(j *subagentJob, history []api.Message) {
	j.setHistory(history)
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.retained[:0]
	for _, id := range s.retained {
		if id != j.id {
			kept = append(kept, id)
		}
	}
	s.retained = append(kept, j.id)
	for len(s.retained) > maxRetainedSubagentHistories {
		oldest := s.retained[0]
		s.retained = s.retained[1:]
		if oj, ok := s.jobs[oldest]; ok {
			oj.setHistory(nil)
		}
	}
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

// cancelAll stops every running job (interrupt path). Jobs finish with a
// cancelled report and still deliver their completion notification, but the
// interrupted flag keeps that notification from auto-waking the parent.
// Cancellation is routed through the unified registry so its record settles
// as killed; the registry's cancel hook is the job's own context cancel.
func (s *subagentStore) cancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if done, _, _ := j.snapshot(); !done && j.cancel != nil {
			j.mu.Lock()
			j.interrupted = true
			j.mu.Unlock()
			if _, err := jobs.Default().Cancel(j.id); err != nil {
				j.cancel() // unregistered job (tests): cancel directly
			}
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

// subagentResume carries a follow-up's linkage: the finished job being
// resumed and its retained conversation, seeded into the new run.
type subagentResume struct {
	jobID   int
	history []api.Message
}

// spawnSubagentAsync starts the tasks as a detached background job and returns
// the handle text immediately. The job's file mutations are banked into the
// parent's /undo checkpoint via the same Before hook the sync path uses — the
// store is mutex-guarded, so the hook is safe on the worker goroutine.
// Ordering caveat: if the parent turn ends before the job finishes, later
// snapshots land in whichever turn checkpoint is open when the write happens
// (a fresh pending bank if none is), so /undo attribution for those detached
// writes belongs to that later checkpoint, not the spawning turn.
//
// When resume is set, the call is a follow-up to a finished sub-agent: the
// child is seeded with the retained conversation (a fresh step budget, same
// system prompt and tool filter), and the new job's label links back to the
// original ("follow-up to job N").
func (m *Model) spawnSubagentAsync(tasks []string, resume *subagentResume) (string, error) {
	m.ensureSubagentRuntime()
	if n := len(m.subagents.running()); n >= maxBackgroundSubagentJobs {
		return "", fmt.Errorf("%d background sub-agent jobs are still running (max %d) — wait for their completion notifications, or spawn with async=false to run inline", n, maxBackgroundSubagentJobs)
	}

	ctx, cancel := context.WithTimeout(context.Background(), subagentJobTimeout)
	job := &subagentJob{tasks: tasks, started: time.Now(), cancel: cancel}

	label := tasks[0]
	if len(tasks) > 1 {
		label = fmt.Sprintf("%d tasks", len(tasks))
	} else if resume != nil {
		label = fmt.Sprintf("follow-up to job %d: %s", resume.jobID, tasks[0])
	}
	// Register into the unified job registry BEFORE starting the worker: the
	// returned id is the job id the model sees, and it can never collide with
	// a background shell job's id. Cancel maps onto the job's context cancel;
	// output is the final report (or a still-running note) — see the hooks.
	job.rec = jobs.Default().Register(jobs.KindSubagent, truncatePlain(label, 80), jobs.Hooks{
		Status: job.statusLine,
		Output: job.outputSnapshot,
		Cancel: cancel,
	})
	job.id = job.rec.ID()
	m.subagents.mu.Lock()
	m.subagents.jobs[job.id] = job
	m.subagents.mu.Unlock()

	opts := m.subagentOptions(m.checkpointBeforeCall(), job.markMutated)
	if resume != nil {
		opts.PriorMessages = resume.history
	}

	go func() {
		defer cancel()
		report, history, err := m.runSubagentTasks(ctx, tasks, opts)
		// Retain whatever conversation the child produced — even a failed,
		// timed-out, or killed run — so the parent can send it a follow-up.
		if len(history) > 0 {
			m.subagents.retain(job, history)
		}
		switch {
		case err != nil:
			report = "(failed: " + err.Error() + ")"
			job.rec.Fail(truncatePlain(err.Error(), 80))
		case ctx.Err() == context.DeadlineExceeded:
			// The job's own timeout fired: failed, not killed. (The report
			// text keeps the historical "(cancelled)" prefix.)
			report = "(cancelled)\n\n" + report
			job.rec.Fail("timed out")
		case ctx.Err() != nil:
			// A user interrupt or a job_kill. Both funnel through cancel; the
			// registry rewrites this Finish to killed when the cancel came
			// through it (always, today — cancelAll routes there too).
			report = "(cancelled)\n\n" + report
			job.rec.Finish("cancelled")
		default:
			job.rec.Finish("")
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

	return fmt.Sprintf("Spawned background sub-agent job %d (%s). It is running now; you will receive its report(s) as a completion notification — do NOT respawn or wait idly, continue with other work or tell the user what is underway. Task: %s",
		job.id, label, truncatePlain(strings.Join(tasks, " | "), 120)), nil
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

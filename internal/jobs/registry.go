// Package jobs is the unified background-job registry. Background shell
// commands (tools/shell_bg.go), background sub-agent jobs
// (tui/subagent_bg.go) and persistent terminal sessions (tools/terminal.go)
// register into ONE shared id space, so "job 3" is unambiguous no matter which
// producer started it. The registry owns identity and lifecycle state;
// producers own the execution resources and report through Hooks. The
// model-facing job_list / job_output / job_kill tools live in tools/jobs.go.
package jobs

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Kind names the producer that started a job.
type Kind string

const (
	KindShell    Kind = "shell"
	KindSubagent Kind = "subagent"
	KindTerminal Kind = "terminal"
)

// Status is the lifecycle state of a job: running, then exactly one terminal
// status. The first settle wins; a cancel requested through the registry
// rewrites the terminal status to killed.
type Status string

const (
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusKilled  Status = "killed"
	StatusFailed  Status = "failed"
)

// ErrCancelUnsupported is returned by Registry.Cancel when the job's producer
// supplied no cancel hook.
var ErrCancelUnsupported = errors.New("job does not support cancellation")

// Hooks connect a registry record to the producer's execution resources. Any
// hook may be nil: nil Status falls back to the lifecycle state, nil Output
// reads as no output, and nil Cancel makes the job unkillable through the
// registry (ErrCancelUnsupported). Hooks are called WITHOUT the job lock held
// and must be goroutine-safe.
type Hooks struct {
	// Status renders a live one-line status (e.g. "running (pid 123, 5s
	// elapsed)", "exited 0").
	Status func() string
	// Output returns a snapshot of the job's accumulated output (a shell
	// job's stream) or its final result (a sub-agent report; a "still
	// running" note while live).
	Output func() string
	// Cancel requests termination. It must be idempotent; settlement still
	// comes from the producer calling Finish/Fail afterwards.
	Cancel func()
}

// Job is one registered background job. Producers settle it with Finish/Fail;
// readers use the accessor methods. All methods are goroutine-safe.
type Job struct {
	id      int
	kind    Kind
	label   string
	started time.Time
	hooks   Hooks

	mu              sync.Mutex
	status          Status
	detail          string
	cancelRequested bool
}

func (j *Job) ID() int            { return j.id }
func (j *Job) Kind() Kind         { return j.kind }
func (j *Job) Label() string      { return j.label }
func (j *Job) Started() time.Time { return j.started }

// State returns the lifecycle state.
func (j *Job) State() Status {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status
}

// Detail returns the producer-supplied status detail (exit code, error
// summary), usually set at settlement.
func (j *Job) Detail() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.detail
}

// StatusLine renders the live producer status when a Status hook exists, else
// the lifecycle state with any detail.
func (j *Job) StatusLine() string {
	if j.hooks.Status != nil {
		return j.hooks.Status()
	}
	state, detail := j.State(), j.Detail()
	if detail != "" {
		return fmt.Sprintf("%s (%s)", state, detail)
	}
	return string(state)
}

// Output returns the producer's output snapshot, or "" when the producer
// supplied no output hook.
func (j *Job) Output() string {
	if j.hooks.Output == nil {
		return ""
	}
	return j.hooks.Output()
}

// Finish settles the job as done; Fail settles it as failed. A cancel
// requested through Registry.Cancel rewrites either to killed. Only the first
// settle takes effect.
func (j *Job) Finish(detail string) { j.settle(StatusDone, detail) }
func (j *Job) Fail(detail string)   { j.settle(StatusFailed, detail) }

func (j *Job) settle(s Status, detail string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status != StatusRunning {
		return
	}
	if j.cancelRequested {
		s = StatusKilled
	}
	j.status = s
	j.detail = detail
}

// Registry issues ids from ONE shared counter for every kind and tracks the
// live set. Use New (or the process-global Default).
type Registry struct {
	mu   sync.Mutex
	jobs map[int]*Job
	next int
}

// New returns an empty registry whose first job gets id 1.
func New() *Registry {
	return &Registry{jobs: map[int]*Job{}, next: 1}
}

// Register adds a job in the running state and returns it. The id is unique
// across all kinds in this registry.
func (r *Registry) Register(kind Kind, label string, hooks Hooks) *Job {
	j := &Job{id: 0, kind: kind, label: label, started: time.Now(), status: StatusRunning, hooks: hooks}
	r.mu.Lock()
	j.id = r.next
	r.next++
	r.jobs[j.id] = j
	r.mu.Unlock()
	return j
}

// Get returns the job with id, or false when no such job exists.
func (r *Registry) Get(id int) (*Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	return j, ok
}

// List returns every job, ordered by id.
func (r *Registry) List() []*Job {
	r.mu.Lock()
	out := make([]*Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, j)
	}
	r.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].id < out[b].id })
	return out
}

// Cancel requests termination of a running job through its producer hook and
// returns the job. A job already in a terminal state is returned without
// invoking the hook (read State to report "already finished"); a job with no
// cancel hook yields ErrCancelUnsupported. The job keeps State running until
// the producer settles it.
func (r *Registry) Cancel(id int) (*Job, error) {
	j, ok := r.Get(id)
	if !ok {
		return nil, fmt.Errorf("no job %d", id)
	}
	j.mu.Lock()
	if j.status != StatusRunning {
		j.mu.Unlock()
		return j, nil
	}
	if j.hooks.Cancel == nil {
		j.mu.Unlock()
		return j, ErrCancelUnsupported
	}
	j.cancelRequested = true
	cancel := j.hooks.Cancel
	j.mu.Unlock()
	cancel()
	return j, nil
}

// defaultRegistry is the process-global registry both producers share: shell
// jobs are package-global and sub-agent jobs are per-Model singletons, and the
// process has one conversation surface, so one global id space fits.
var defaultRegistry = New()

// Default returns the process-global registry.
func Default() *Registry { return defaultRegistry }

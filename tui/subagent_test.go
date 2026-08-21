package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/internal/jobs"
	"github.com/javanhut/ollama_code/tools"
)

// subagentTestModel builds the minimal Model the spawn paths touch: registry,
// notes, transcript renderers, and the turn-guard maps. agentRunner is left
// nil here — each test installs its own fake.
func subagentTestModel() *Model {
	return &Model{
		mode:        ExploreMode,
		state:       stateChat,
		tools:       tools.DefaultRegistry(),
		notes:       &sessionNotes{},
		transcript:  &strings.Builder{},
		streamBuf:   &strings.Builder{},
		md:          newMarkdownRenderer(),
		notesMd:     newMarkdownRenderer(),
		failedCalls: make(map[string]int),
		maxSteps:    25,
		input:       textarea.New(),
	}
}

// awaitJob receives the next completed background job, failing the test if
// none arrives promptly.
func awaitJob(t *testing.T, m *Model) *subagentJob {
	t.Helper()
	select {
	case job := <-m.subagentEvents:
		return job
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for sub-agent completion event")
		return nil
	}
}

func TestSubagentCallIsAsync(t *testing.T) {
	if !subagentCallIsAsync(toolCall("spawn_subagent", `{"task":"x"}`)) {
		t.Fatal("async must default to true")
	}
	if subagentCallIsAsync(toolCall("spawn_subagent", `{"task":"x","async":false}`)) {
		t.Fatal("explicit async=false must be sync")
	}
	if !subagentCallIsAsync(toolCall("spawn_subagent", `{"task":"x","async":true}`)) {
		t.Fatal("explicit async=true must be async")
	}
	if subagentCallIsAsync(toolCall("write_file", `{}`)) {
		t.Fatal("non-spawn calls are never async spawns")
	}
}

// TestSpawnSubagentAsyncReturnsImmediately is the core of the feature: the
// tool call must return a job handle right away even though the sub-agent is
// still running.
func TestSpawnSubagentAsyncReturnsImmediately(t *testing.T) {
	m := subagentTestModel()
	release := make(chan struct{})
	started := make(chan struct{})
	m.agentRunner = func(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		close(started)
		select {
		case <-release:
			return agent.Result{Output: "report for: " + task}, nil
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		}
	}
	tool := m.spawnSubagentTool()

	start := time.Now()
	res, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"investigate foo"}`))
	if err != nil {
		t.Fatalf("async spawn returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("async spawn blocked for %s", elapsed)
	}
	if !strings.Contains(res, "job 1") {
		t.Fatalf("expected a job handle in the result, got %q", res)
	}
	<-started // the worker is actually running behind the handle
	if got := m.runningSubagentJobs(); got != 1 {
		t.Fatalf("expected 1 running job, got %d", got)
	}

	close(release)
	job := awaitJob(t, m)
	done, _, report := job.snapshot()
	if !done {
		t.Fatal("job not marked done after completion")
	}
	if !strings.Contains(report, "report for: investigate foo") {
		t.Fatalf("unexpected report %q", report)
	}
	if got := m.runningSubagentJobs(); got != 0 {
		t.Fatalf("expected no running jobs after completion, got %d", got)
	}
}

// TestSubagentCompletionNotifies covers the delivery half: a finished job's
// report is injected into the conversation as a system message, a toast is
// set, the event waiter is re-armed, and an idle parent is woken with a new
// stream so it can react to the report.
func TestSubagentCompletionNotifies(t *testing.T) {
	m := subagentTestModel()
	m.modelName = "test-model" // non-empty so the idle wake path can fire

	job := &subagentJob{id: 7, tasks: []string{"investigate foo"}, started: time.Now()}
	job.finish("found the thing in foo.go")

	_, cmd := m.Update(subagentDoneMsg{job: job})

	if len(m.history) != 1 {
		t.Fatalf("expected the completion notification in history, got %#v", m.history)
	}
	msg := m.history[0]
	if msg.Role != "system" ||
		!strings.Contains(msg.Content, "[SUB-AGENT JOB 7 COMPLETE]") ||
		!strings.Contains(msg.Content, "found the thing in foo.go") {
		t.Fatalf("unexpected notification message: %#v", msg)
	}
	if !strings.Contains(m.toast, "sub-agent job 7 finished") {
		t.Fatalf("unexpected toast %q", m.toast)
	}
	if cmd == nil {
		t.Fatal("expected commands (re-armed waiter + wake stream)")
	}
	if !m.streaming || m.stream == nil {
		t.Fatal("idle parent should have been woken with a new stream")
	}
	m.stream.cancel()
}

// TestSubagentCompletionDoesNotWakeBusyOrInterrupted verifies the two cases
// where the completion must not kick a new stream: a turn already in flight
// (the parent sees the notification at its next step) and a user-cancelled
// job (esc meant stop).
func TestSubagentCompletionDoesNotWakeBusyOrInterrupted(t *testing.T) {
	// Busy parent: notification lands in history, no new stream.
	m := subagentTestModel()
	m.modelName = "test-model"
	m.streaming = true
	m.stream = &streamState{cancel: func() {}}
	job := &subagentJob{id: 1, tasks: []string{"t"}, started: time.Now()}
	job.finish("report")
	m.Update(subagentDoneMsg{job: job})
	if len(m.history) != 1 || !strings.Contains(m.history[0].Content, "JOB 1 COMPLETE") {
		t.Fatalf("notification missing while busy: %#v", m.history)
	}
	// The pre-existing stream must be untouched (no second stream started).
	if m.stream == nil || m.stream.cancel == nil {
		t.Fatal("busy parent's stream was disturbed")
	}

	// Interrupted job: recorded, toasted, but no wake even when idle.
	m2 := subagentTestModel()
	m2.modelName = "test-model"
	job2 := &subagentJob{id: 2, tasks: []string{"t"}, started: time.Now(), interrupted: true}
	job2.finish("(cancelled)")
	m2.Update(subagentDoneMsg{job: job2})
	if m2.streaming || m2.stream != nil {
		t.Fatal("interrupted job must not wake the parent")
	}
	if !strings.Contains(m2.history[0].Content, "JOB 2 CANCELLED") {
		t.Fatalf("expected a cancellation notice, got %#v", m2.history[0])
	}
}

// TestSpawnSubagentSyncMode keeps the old blocking contract for async=false:
// the handler returns the inline report, no job is registered.
func TestSpawnSubagentSyncMode(t *testing.T) {
	m := subagentTestModel()
	m.agentRunner = func(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		return agent.Result{Output: "did: " + task}, nil
	}
	tool := m.spawnSubagentTool()

	res, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"single","async":false}`))
	if err != nil {
		t.Fatalf("sync spawn returned error: %v", err)
	}
	if res != "did: single" {
		t.Fatalf("expected the inline report, got %q", res)
	}

	res, err = tool.Handler(context.Background(), json.RawMessage(`{"tasks":["a","b"],"async":false}`))
	if err != nil {
		t.Fatalf("sync parallel spawn returned error: %v", err)
	}
	if !strings.Contains(res, "### Sub-agent 1") || !strings.Contains(res, "did: a") || !strings.Contains(res, "did: b") {
		t.Fatalf("expected combined parallel report, got %q", res)
	}
	if got := m.runningSubagentJobs(); got != 0 {
		t.Fatalf("sync mode must not register background jobs, got %d", got)
	}
}

// TestSpawnSubagentBackgroundCheckpointBanking locks in the /undo contract for
// background writes: the Before hook runs on the worker goroutine now, and its
// snapshots must still land in the parent's checkpoint bank so /undo restores
// the pre-delegation content.
func TestSpawnSubagentBackgroundCheckpointBanking(t *testing.T) {
	dir := ckptWorkspace(t)
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := subagentTestModel()
	m.agentRunner = func(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		// Simulate the delegated write exactly as the executor drives it:
		// Before hook first (snapshots), then the mutation.
		if opts.Before == nil {
			t.Error("background job lost the checkpoint Before hook")
			return agent.Result{}, nil
		}
		opts.Before(toolCall("write_file", `{"path":"`+f+`","content":"changed"}`))
		if err := os.WriteFile(f, []byte("changed"), 0o644); err != nil {
			return agent.Result{}, err
		}
		return agent.Result{Output: "wrote " + f}, nil
	}
	tool := m.spawnSubagentTool()

	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"write a.txt"}`)); err != nil {
		t.Fatalf("async spawn returned error: %v", err)
	}
	job := awaitJob(t, m)
	if _, mutated, _ := job.snapshot(); !mutated {
		t.Fatal("job did not record that it mutated files")
	}
	if got, _ := os.ReadFile(f); string(got) != "changed" {
		t.Fatalf("background write did not land, got %q", got)
	}

	m.finalizeCheckpoint("bg delegation")
	if _, touched := m.undoLast(); len(touched) != 1 {
		t.Fatalf("expected undo to touch 1 file, got %v", touched)
	}
	if got, _ := os.ReadFile(f); string(got) != "original" {
		t.Fatalf("undo did not restore the background write, got %q", got)
	}
}

// TestSubagentCompletionArmsVerifyGate: when a background job mutated files in
// write/auto mode, the completion marks the turn as file-touching so the
// verification gate runs (the spawn-time conservative mark can't see async
// writes).
func TestSubagentCompletionArmsVerifyGate(t *testing.T) {
	m := subagentTestModel()
	m.mode = WriteMode
	job := &subagentJob{id: 3, tasks: []string{"edit"}, started: time.Now()}
	job.markMutated()
	job.finish("edited foo.go")
	m.Update(subagentDoneMsg{job: job})
	if !m.turnTouchedFiles {
		t.Fatal("mutating background job did not arm the verification gate")
	}
}

// TestCancelSubagentsInterrupts covers esc semantics: cancelling the store
// cancels the workers' contexts and flags the jobs interrupted, so their
// completion notifications are recorded without waking the parent.
func TestCancelSubagentsInterrupts(t *testing.T) {
	m := subagentTestModel()
	started := make(chan struct{})
	m.agentRunner = func(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		close(started)
		<-ctx.Done()
		return agent.Result{}, ctx.Err()
	}
	tool := m.spawnSubagentTool()
	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"long task"}`)); err != nil {
		t.Fatalf("async spawn returned error: %v", err)
	}
	<-started

	m.cancelSubagents()

	job := awaitJob(t, m)
	if !job.wasInterrupted() {
		t.Fatal("cancelled job not flagged interrupted")
	}
	done, _, report := job.snapshot()
	if !done || !strings.Contains(report, "cancel") {
		t.Fatalf("expected a cancelled completion, done=%v report=%q", done, report)
	}
	if got := m.runningSubagentJobs(); got != 0 {
		t.Fatalf("expected no running jobs after cancel, got %d", got)
	}
}

// TestSpawnSubagentJobCap bounds the background fleet: with the maximum number
// of jobs still running, another async spawn is rejected (the model is told to
// wait or run inline).
func TestSpawnSubagentJobCap(t *testing.T) {
	m := subagentTestModel()
	release := make(chan struct{})
	m.agentRunner = func(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		select {
		case <-release:
			return agent.Result{Output: "ok"}, nil
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		}
	}
	tool := m.spawnSubagentTool()

	for i := range maxBackgroundSubagentJobs {
		if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"t"}`)); err != nil {
			t.Fatalf("spawn %d failed: %v", i+1, err)
		}
	}
	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"one too many"}`)); err == nil ||
		!strings.Contains(err.Error(), "still running") {
		t.Fatalf("expected a capacity error, got %v", err)
	}

	// Once a job finishes, capacity frees up.
	close(release)
	for range maxBackgroundSubagentJobs {
		awaitJob(t, m)
	}
	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"now it fits"}`)); err != nil {
		t.Fatalf("spawn after completions failed: %v", err)
	}
	m.cancelSubagents()
	awaitJob(t, m)
}

// TestInterruptCancelsBackgroundSubagents wires the mid-turn esc/ctrl+c path:
// interrupting a turn must cancel running background jobs too.
func TestInterruptCancelsBackgroundSubagents(t *testing.T) {
	m := subagentTestModel()
	m.modelName = "test-model"
	started := make(chan struct{})
	m.agentRunner = func(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		close(started)
		<-ctx.Done()
		return agent.Result{}, ctx.Err()
	}
	tool := m.spawnSubagentTool()
	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"bg work"}`)); err != nil {
		t.Fatalf("async spawn returned error: %v", err)
	}
	<-started

	m.streaming = true
	m.stream = &streamState{cancel: func() {}}
	m.cancelSubagents() // what the esc/ctrl+c handlers do before interruptTurn
	m.interruptTurn()

	job := awaitJob(t, m)
	if !job.wasInterrupted() {
		t.Fatal("mid-turn interrupt did not flag the job interrupted")
	}
}

// TestSpawnSubagentRegistersInUnifiedRegistry: an async spawn registers a
// subagent-kind record in the shared job registry under the same id the model
// sees, and the record settles with the final report as its output.
func TestSpawnSubagentRegistersInUnifiedRegistry(t *testing.T) {
	m := subagentTestModel()
	m.agentRunner = func(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		return agent.Result{Output: "report for: " + task}, nil
	}
	tool := m.spawnSubagentTool()
	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"investigate foo"}`)); err != nil {
		t.Fatalf("async spawn returned error: %v", err)
	}
	job := awaitJob(t, m)

	rec, ok := jobs.Default().Get(job.id)
	if !ok {
		t.Fatalf("job %d is not in the unified registry", job.id)
	}
	if rec.Kind() != jobs.KindSubagent {
		t.Fatalf("expected kind subagent, got %s", rec.Kind())
	}
	if rec.State() != jobs.StatusDone {
		t.Fatalf("expected the record settled done, got %s", rec.State())
	}
	if !strings.Contains(rec.Label(), "investigate foo") {
		t.Fatalf("label should carry the task summary, got %q", rec.Label())
	}
	if !strings.Contains(rec.Output(), "report for: investigate foo") {
		t.Fatalf("registry output should be the final report, got %q", rec.Output())
	}
}

// TestJobKillCancelsSubagentJob: job_kill (registry cancel) stops a running
// sub-agent job through its context cancel, settles the record as killed, and
// — unlike the user's esc interrupt — does NOT flag the job interrupted, so
// the completion notification flow is unchanged.
func TestJobKillCancelsSubagentJob(t *testing.T) {
	m := subagentTestModel()
	started := make(chan struct{})
	m.agentRunner = func(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		close(started)
		<-ctx.Done()
		return agent.Result{}, ctx.Err()
	}
	tool := m.spawnSubagentTool()
	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"long task"}`)); err != nil {
		t.Fatalf("async spawn returned error: %v", err)
	}
	<-started

	running := m.subagents.running()
	if len(running) != 1 {
		t.Fatalf("expected 1 running job, got %d", len(running))
	}
	id := running[0].id
	if _, err := jobs.Default().Cancel(id); err != nil {
		t.Fatalf("registry cancel failed: %v", err)
	}

	job := awaitJob(t, m)
	if job.id != id {
		t.Fatalf("completed job %d, expected %d", job.id, id)
	}
	if job.wasInterrupted() {
		t.Fatal("a job_kill must not mark the job user-interrupted")
	}
	rec, ok := jobs.Default().Get(id)
	if !ok || rec.State() != jobs.StatusKilled {
		t.Fatalf("expected the record settled as killed, got ok=%v state=%s", ok, rec.State())
	}
	done, _, report := job.snapshot()
	if !done || !strings.Contains(report, "cancel") {
		t.Fatalf("expected a cancelled completion, done=%v report=%q", done, report)
	}
}

// TestUserInterruptSettlesRegistryKilled: esc/ctrl+c (cancelSubagents) routes
// through the registry too, so its record shows killed while the notification
// keeps the interrupted, no-auto-wake semantics.
func TestUserInterruptSettlesRegistryKilled(t *testing.T) {
	m := subagentTestModel()
	started := make(chan struct{})
	m.agentRunner = func(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		close(started)
		<-ctx.Done()
		return agent.Result{}, ctx.Err()
	}
	tool := m.spawnSubagentTool()
	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"bg work"}`)); err != nil {
		t.Fatalf("async spawn returned error: %v", err)
	}
	<-started

	m.cancelSubagents()
	job := awaitJob(t, m)
	if !job.wasInterrupted() {
		t.Fatal("esc interrupt must still flag the job interrupted")
	}
	rec, ok := jobs.Default().Get(job.id)
	if !ok || rec.State() != jobs.StatusKilled {
		t.Fatalf("expected the record settled as killed, got ok=%v state=%s", ok, rec.State())
	}
}

// TestShellAndSubagentJobsShareIDSpace: a background shell job (started
// through the public run_shell tool) and a background sub-agent job draw ids
// from the same counter, so "job N" is unambiguous across kinds.
func TestShellAndSubagentJobsShareIDSpace(t *testing.T) {
	before := 0
	for _, j := range jobs.Default().List() {
		before = max(before, j.ID())
	}

	shellRes, err := tools.RunShellTool().Handler(context.Background(),
		json.RawMessage(`{"command":"sleep 30","background":true}`))
	if err != nil {
		t.Fatalf("background run_shell failed: %v", err)
	}
	if !strings.Contains(shellRes, "started background job") {
		t.Fatalf("unexpected run_shell result: %q", shellRes)
	}

	m := subagentTestModel()
	started := make(chan struct{})
	m.agentRunner = func(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		close(started)
		<-ctx.Done()
		return agent.Result{}, ctx.Err()
	}
	if _, err := m.spawnSubagentTool().Handler(context.Background(), json.RawMessage(`{"task":"bg work"}`)); err != nil {
		t.Fatalf("async spawn returned error: %v", err)
	}
	<-started

	var shellJob, subJob *jobs.Job
	for _, j := range jobs.Default().List() {
		if j.ID() <= before {
			continue
		}
		switch j.Kind() {
		case jobs.KindShell:
			shellJob = j
		case jobs.KindSubagent:
			subJob = j
		}
	}
	if shellJob == nil || subJob == nil {
		t.Fatalf("expected one new job of each kind, got shell=%v subagent=%v", shellJob, subJob)
	}
	if shellJob.ID() == subJob.ID() {
		t.Fatalf("shell and sub-agent jobs collided on id %d", shellJob.ID())
	}

	// Clean up: kill both through the shared registry.
	if _, err := jobs.Default().Cancel(shellJob.ID()); err != nil {
		t.Fatalf("cancel shell job: %v", err)
	}
	m.cancelSubagents()
	awaitJob(t, m)
}

// resumeScript is an agentRunner that plays scripted behaviors per call and
// records the opts each call saw (for PriorMessages/budget assertions).
type resumeScript struct {
	mu     sync.Mutex
	calls  int
	seen   []agent.Options
	behave func(call int, ctx context.Context, task string, opts agent.Options) (agent.Result, error)
}

func (s *resumeScript) run(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.seen = append(s.seen, opts)
	s.mu.Unlock()
	return s.behave(call, ctx, task, opts)
}

func (s *resumeScript) optsFor(call int) agent.Options {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[call-1]
}

func historyOf(msgs ...api.Message) []api.Message { return msgs }

// TestSpawnSubagentResumeValidation locks in the model-facing errors for a
// bad resume_job: unknown id, a shell job's id, a still-running sub-agent, a
// parallel multi-task job (no retained history), and resume combined with
// tasks.
func TestSpawnSubagentResumeValidation(t *testing.T) {
	m := subagentTestModel()
	release := make(chan struct{})
	started := make(chan struct{})
	m.agentRunner = func(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-release:
			return agent.Result{Output: "did: " + task}, nil
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		}
	}
	tool := m.spawnSubagentTool()

	// Unknown id.
	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"hi","resume_job":999999}`)); err == nil ||
		!strings.Contains(err.Error(), "no sub-agent job 999999") {
		t.Fatalf("unknown id: expected a clear error, got %v", err)
	}

	// A shell job's id.
	shellRec := jobs.Default().Register(jobs.KindShell, "sleep 30", jobs.Hooks{})
	if _, err := tool.Handler(context.Background(), json.RawMessage(fmt.Sprintf(`{"task":"hi","resume_job":%d}`, shellRec.ID()))); err == nil ||
		!strings.Contains(err.Error(), "shell job, not a sub-agent job") {
		t.Fatalf("shell id: expected a kind error, got %v", err)
	}
	shellRec.Finish("")

	// A still-running sub-agent job.
	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"long task"}`)); err != nil {
		t.Fatalf("async spawn failed: %v", err)
	}
	<-started
	running := m.subagents.running()
	if len(running) != 1 {
		t.Fatalf("expected 1 running job, got %d", len(running))
	}
	runningID := running[0].id
	if _, err := tool.Handler(context.Background(), json.RawMessage(fmt.Sprintf(`{"task":"hi","resume_job":%d}`, runningID))); err == nil ||
		!strings.Contains(err.Error(), "still running") {
		t.Fatalf("running job: expected a wait error, got %v", err)
	}
	close(release)
	job := awaitJob(t, m)

	// resume_job cannot be combined with tasks.
	if _, err := tool.Handler(context.Background(), json.RawMessage(fmt.Sprintf(`{"tasks":["a","b"],"resume_job":%d}`, job.id))); err == nil ||
		!strings.Contains(err.Error(), "exactly one follow-up") {
		t.Fatalf("tasks combo: expected a clear error, got %v", err)
	}

	// A parallel multi-task job retains no per-child history.
	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"tasks":["a","b"]}`)); err != nil {
		t.Fatalf("parallel spawn failed: %v", err)
	}
	multi := awaitJob(t, m)
	if _, err := tool.Handler(context.Background(), json.RawMessage(fmt.Sprintf(`{"task":"hi","resume_job":%d}`, multi.id))); err == nil ||
		!strings.Contains(err.Error(), "no retained conversation") {
		t.Fatalf("multi-task job: expected a no-history error, got %v", err)
	}
}

// TestSpawnSubagentResumeAsync is the core resume flow: a completed
// background child's history is retained, resume_job spawns a NEW job seeded
// with that history plus the follow-up on a fresh step budget, the new job's
// label links back to the original, and completion notifies through
// subagentEvents like any async job.
func TestSpawnSubagentResumeAsync(t *testing.T) {
	m := subagentTestModel()
	hist1 := historyOf(
		api.Message{Role: "system", Content: "sys"},
		api.Message{Role: "user", Content: "investigate foo"},
		api.Message{Role: "assistant", Content: "first report"},
	)
	hist2 := append(append([]api.Message{}, hist1...),
		api.Message{Role: "user", Content: "what about y?"},
		api.Message{Role: "assistant", Content: "follow-up report"},
	)
	script := &resumeScript{behave: func(call int, ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		if call == 1 {
			return agent.Result{Output: "first report", Messages: hist1}, nil
		}
		return agent.Result{Output: "follow-up report", Messages: hist2}, nil
	}}
	m.agentRunner = script.run
	tool := m.spawnSubagentTool()

	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"investigate foo"}`)); err != nil {
		t.Fatalf("async spawn failed: %v", err)
	}
	job1 := awaitJob(t, m)
	if got := job1.historySnapshot(); len(got) != len(hist1) {
		t.Fatalf("completed child's history not retained, got %#v", got)
	}
	// The completion notice points at the resume path.
	if note := job1.notification(); !strings.Contains(note, fmt.Sprintf("resume_job=%d", job1.id)) {
		t.Fatalf("completion notice should hint at resume_job, got %q", note)
	}

	res, err := tool.Handler(context.Background(), json.RawMessage(fmt.Sprintf(`{"task":"what about y?","resume_job":%d}`, job1.id)))
	if err != nil {
		t.Fatalf("async resume failed: %v", err)
	}
	if !strings.Contains(res, "Spawned background sub-agent job") {
		t.Fatalf("expected a new job handle, got %q", res)
	}
	job2 := awaitJob(t, m)
	if job2.id == job1.id {
		t.Fatal("resume must register as a NEW job")
	}

	// The resumed child saw the retained history verbatim, and on a FRESH
	// step budget.
	seed := script.optsFor(2).PriorMessages
	if len(seed) != len(hist1) {
		t.Fatalf("resumed child not seeded with the prior history, got %#v", seed)
	}
	for i, msg := range hist1 {
		if !reflect.DeepEqual(seed[i], msg) {
			t.Fatalf("seed message %d changed: want %#v, got %#v", i, msg, seed[i])
		}
	}
	if got := script.optsFor(2).MaxSteps; got != subagentMaxSteps {
		t.Fatalf("resumed child should get a fresh %d-step budget, got %d", subagentMaxSteps, got)
	}

	// The follow-up ran as the next user message and its report arrived via
	// the normal async completion path.
	done, _, report := job2.snapshot()
	if !done || !strings.Contains(report, "follow-up report") {
		t.Fatalf("unexpected resume completion, done=%v report=%q", done, report)
	}
	// The registry label links the new job to the original.
	rec, ok := jobs.Default().Get(job2.id)
	if !ok || !strings.Contains(rec.Label(), fmt.Sprintf("follow-up to job %d", job1.id)) {
		t.Fatalf("label should link back to job %d, got %q", job1.id, rec.Label())
	}
	// The follow-up's full conversation is itself retained (chaining).
	if got := job2.historySnapshot(); len(got) != len(hist2) {
		t.Fatalf("follow-up history not retained for chaining, got %#v", got)
	}
}

// TestSpawnSubagentResumeSync: async=false resume blocks and returns the
// report inline, and the ORIGINAL job's retained history is updated so
// further follow-ups keep referencing the same job id.
func TestSpawnSubagentResumeSync(t *testing.T) {
	m := subagentTestModel()
	hist1 := historyOf(
		api.Message{Role: "user", Content: "investigate foo"},
		api.Message{Role: "assistant", Content: "first report"},
	)
	hist2 := append(append([]api.Message{}, hist1...),
		api.Message{Role: "user", Content: "and bar?"},
		api.Message{Role: "assistant", Content: "sync follow-up report"},
	)
	script := &resumeScript{behave: func(call int, ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		if call == 1 {
			return agent.Result{Output: "first report", Messages: hist1}, nil
		}
		return agent.Result{Output: "sync follow-up report", Messages: hist2}, nil
	}}
	m.agentRunner = script.run
	tool := m.spawnSubagentTool()

	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"investigate foo"}`)); err != nil {
		t.Fatalf("async spawn failed: %v", err)
	}
	job1 := awaitJob(t, m)

	res, err := tool.Handler(context.Background(), json.RawMessage(fmt.Sprintf(`{"task":"and bar?","resume_job":%d,"async":false}`, job1.id)))
	if err != nil {
		t.Fatalf("sync resume failed: %v", err)
	}
	if res != "sync follow-up report" {
		t.Fatalf("sync resume should return the inline report, got %q", res)
	}
	if got := script.optsFor(2).PriorMessages; len(got) != len(hist1) {
		t.Fatalf("sync resume not seeded with the prior history, got %#v", got)
	}
	// No new background job was registered, and the original id now carries
	// the extended history.
	if got := m.runningSubagentJobs(); got != 0 {
		t.Fatalf("sync resume must not register background jobs, got %d", got)
	}
	if got := job1.historySnapshot(); len(got) != len(hist2) {
		t.Fatalf("original job's history not updated, got %#v", got)
	}
}

// TestSubagentHistoryRetentionCap bounds retained histories: completing more
// than the cap evicts the oldest, and resuming an evicted child fails with a
// clear error while the newest stay resumable.
func TestSubagentHistoryRetentionCap(t *testing.T) {
	m := subagentTestModel()
	m.agentRunner = func(ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		return agent.Result{Output: "did: " + task, Messages: historyOf(
			api.Message{Role: "user", Content: task},
			api.Message{Role: "assistant", Content: "did: " + task},
		)}, nil
	}
	tool := m.spawnSubagentTool()

	total := maxRetainedSubagentHistories + 2
	completed := make([]*subagentJob, 0, total)
	for i := 0; i < total; i++ {
		if _, err := tool.Handler(context.Background(), json.RawMessage(fmt.Sprintf(`{"task":"task %d"}`, i))); err != nil {
			t.Fatalf("spawn %d failed: %v", i, err)
		}
		completed = append(completed, awaitJob(t, m))
	}

	for i, job := range completed {
		got := len(job.historySnapshot())
		if i < 2 && got != 0 {
			t.Fatalf("job %d (oldest) should have been evicted, still has %d messages", job.id, got)
		}
		if i >= 2 && got == 0 {
			t.Fatalf("job %d should still be retained", job.id)
		}
	}
	if _, err := tool.Handler(context.Background(), json.RawMessage(fmt.Sprintf(`{"task":"hi","resume_job":%d}`, completed[0].id))); err == nil ||
		!strings.Contains(err.Error(), "no retained conversation") {
		t.Fatalf("evicted job: expected a clear error, got %v", err)
	}
	newest := completed[total-1]
	if _, err := tool.Handler(context.Background(), json.RawMessage(fmt.Sprintf(`{"task":"hi","resume_job":%d,"async":false}`, newest.id))); err != nil {
		t.Fatalf("newest job should still resume: %v", err)
	}
}

// TestResumeKilledSubagent: a user-interrupted child keeps the history up to
// the interruption and can be resumed from it.
func TestResumeKilledSubagent(t *testing.T) {
	m := subagentTestModel()
	started := make(chan struct{})
	partial := historyOf(
		api.Message{Role: "user", Content: "long task"},
		api.Message{Role: "assistant", Content: "partial work"},
	)
	script := &resumeScript{behave: func(call int, ctx context.Context, task string, opts agent.Options) (agent.Result, error) {
		if call == 1 {
			close(started)
			<-ctx.Done()
			return agent.Result{Messages: partial}, ctx.Err()
		}
		return agent.Result{Output: "resumed after kill", Messages: append(partial,
			api.Message{Role: "user", Content: task},
			api.Message{Role: "assistant", Content: "resumed after kill"},
		)}, nil
	}}
	m.agentRunner = script.run
	tool := m.spawnSubagentTool()

	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"task":"long task"}`)); err != nil {
		t.Fatalf("async spawn failed: %v", err)
	}
	<-started
	m.cancelSubagents()
	job1 := awaitJob(t, m)
	if !job1.wasInterrupted() {
		t.Fatal("expected the killed job to be flagged interrupted")
	}
	if got := job1.historySnapshot(); len(got) != len(partial) {
		t.Fatalf("killed child's partial history not retained, got %#v", got)
	}

	if _, err := tool.Handler(context.Background(), json.RawMessage(fmt.Sprintf(`{"task":"finish it","resume_job":%d}`, job1.id))); err != nil {
		t.Fatalf("resume of killed child failed: %v", err)
	}
	job2 := awaitJob(t, m)
	done, _, report := job2.snapshot()
	if !done || !strings.Contains(report, "resumed after kill") {
		t.Fatalf("unexpected resume-of-kill completion, done=%v report=%q", done, report)
	}
	if seed := script.optsFor(2).PriorMessages; len(seed) != len(partial) {
		t.Fatalf("resumed-from-kill child not seeded with the partial history, got %#v", seed)
	}
}

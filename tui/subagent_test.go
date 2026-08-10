package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"

	"github.com/javanhut/ollama_code/internal/agent"
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
	dir := t.TempDir()
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

	for i := 0; i < maxBackgroundSubagentJobs; i++ {
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
	for i := 0; i < maxBackgroundSubagentJobs; i++ {
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

package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/jobs"
)

// isolateJobs points the /jobs modal at a fresh registry, so a background shell
// started by another test in this process cannot appear in these rows.
func isolateJobs(t *testing.T) {
	t.Helper()
	reg := jobs.New()
	jobRegistry = func() *jobs.Registry { return reg }
	t.Cleanup(func() { jobRegistry = jobs.Default })
}

func TestJobsModalEmpty(t *testing.T) {
	isolateJobs(t)
	m := newSized(t)
	m.state = stateJobs
	if out := m.jobsModal(); !strings.Contains(out, "no background jobs") {
		t.Fatalf("empty modal should say so, got:\n%s", out)
	}
}

func TestJobsModalListsSubagents(t *testing.T) {
	isolateJobs(t)
	// Wide enough that the row doesn't truncate the task text.
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	m := mm.(*Model)
	m.ensureSubagentRuntime()
	m.subagents.jobs[1] = &subagentJob{id: 1, tasks: []string{"explore the repo"}, started: time.Now()}

	out := m.jobsModal()
	if !strings.Contains(out, "explore the repo") {
		t.Fatalf("modal should list the sub-agent task, got:\n%s", out)
	}
	if !strings.Contains(out, "agent") || !strings.Contains(out, "running") {
		t.Fatalf("modal should show kind and status, got:\n%s", out)
	}
}

// cancel(id) stops one running job and refuses unknown or finished ids.
func TestSubagentCancelByID(t *testing.T) {
	s := newSubagentStore()
	cancelled := false
	j := &subagentJob{id: 1, tasks: []string{"t"}, started: time.Now(), cancel: func() { cancelled = true }}
	s.jobs[1] = j

	if !s.cancel(1) {
		t.Fatal("cancel(1) should report true for a running job")
	}
	if !cancelled {
		t.Fatal("cancel func was not invoked")
	}
	if !j.wasInterrupted() {
		t.Fatal("cancelled job should be marked interrupted")
	}
	if s.cancel(2) {
		t.Fatal("unknown id should return false")
	}

	finished := &subagentJob{id: 2, tasks: []string{"t"}, started: time.Now()}
	finished.finish("done")
	s.jobs[2] = finished
	if s.cancel(2) {
		t.Fatal("finished job should return false")
	}
}

// all() returns every job, running or finished, sorted by id.
func TestSubagentAllSnapshot(t *testing.T) {
	s := newSubagentStore()
	finished := &subagentJob{id: 2, tasks: []string{"b"}, started: time.Now()}
	finished.finish("done")
	s.jobs[2] = finished
	s.jobs[1] = &subagentJob{id: 1, tasks: []string{"a"}, started: time.Now()}

	all := s.all()
	if len(all) != 2 || all[0].id != 1 || all[1].id != 2 {
		t.Fatalf("all() = %d jobs out of order: %v", len(all), all)
	}
	if n := len(s.running()); n != 1 {
		t.Fatalf("running() = %d, want 1", n)
	}
}

// /jobs opens the modal, x cancels the highlighted sub-agent job, esc closes.
func TestJobsModalKeys(t *testing.T) {
	isolateJobs(t)
	m := typeKeys(t, newSized(t), "/jobs")
	m = press(t, m, tea.KeyEnter, 0)
	if m.state != stateJobs {
		t.Fatalf("state %v, want stateJobs", m.state)
	}

	cancelled := false
	m.ensureSubagentRuntime()
	m.subagents.jobs[1] = &subagentJob{id: 1, tasks: []string{"t"}, started: time.Now(), cancel: func() { cancelled = true }}

	m = typeKeys(t, m, "x")
	if !cancelled {
		t.Fatal("x should cancel the highlighted sub-agent job")
	}
	if !strings.Contains(m.toast, "cancelled") {
		t.Fatalf("toast %q should confirm the cancel", m.toast)
	}

	// A finished job is reported, not re-killed.
	m = typeKeys(t, m, "x")
	if !strings.Contains(m.toast, "no running sub-agent job") {
		t.Fatalf("toast %q should refuse the second kill", m.toast)
	}

	m = press(t, m, tea.KeyEscape, 0)
	if m.state != stateChat {
		t.Fatalf("esc left state at %v, want stateChat", m.state)
	}
}

// The modal can be opened mid-turn, and esc there means "close the modal",
// not "interrupt the turn".
func TestJobsModalEscDoesNotInterruptStream(t *testing.T) {
	m := newSized(t)
	m.state = stateJobs
	m.streaming = true
	m.stream = &streamState{gen: m.turnGen}

	m = press(t, m, tea.KeyEscape, 0)
	if m.state != stateChat {
		t.Fatalf("esc left state at %v, want stateChat", m.state)
	}
	if !m.streaming {
		t.Fatal("esc in the jobs modal interrupted the turn")
	}
}

// /compact forces a summary pass on demand. It reports rather than silently
// doing nothing when there is not enough history to halve.
func TestCompactCommand(t *testing.T) {
	m := typeKeys(t, newSized(t), "/compact")
	m = press(t, m, tea.KeyEnter, 0)
	if m.compacting {
		t.Fatal("an empty session has nothing to compact")
	}
	if !strings.Contains(m.toast, "too short") {
		t.Fatalf("toast %q should explain why nothing happened", m.toast)
	}

	for i := range 8 {
		m.history = append(m.history,
			api.Message{Role: "user", Content: fmt.Sprintf("question %d", i)},
			api.Message{Role: "assistant", Content: fmt.Sprintf("answer %d", i)})
	}
	m = typeKeys(t, m, "/compact")
	m = press(t, m, tea.KeyEnter, 0)
	if !m.compacting {
		t.Fatalf("compaction did not start; toast %q", m.toast)
	}
}

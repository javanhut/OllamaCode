package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestJobsModalEmpty(t *testing.T) {
	m := newSized(t)
	m.state = stateJobs
	out := m.jobsModal()
	if !strings.Contains(out, "no background jobs") {
		t.Fatalf("empty modal should say so, got:\n%s", out)
	}
}

func TestJobsModalListsSubagents(t *testing.T) {
	// Wide enough that the row doesn't truncate the task text.
	mm, _ := New().Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	m := mm.(*Model)
	m.ensureSubagentRuntime()
	m.subagents.jobs[1] = &subagentJob{id: 1, tasks: []string{"explore the repo"}, started: time.Now()}
	m.subagents.next = 2
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
	s.next = 2

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

	j2 := &subagentJob{id: 2, tasks: []string{"t"}, started: time.Now()}
	j2.finish("done")
	s.jobs[2] = j2
	if s.cancel(2) {
		t.Fatal("finished job should return false")
	}
}

// all() returns every job, running or finished, sorted by id.
func TestSubagentAllSnapshot(t *testing.T) {
	s := newSubagentStore()
	running := &subagentJob{id: 1, tasks: []string{"a"}, started: time.Now()}
	finished := &subagentJob{id: 2, tasks: []string{"b"}, started: time.Now()}
	finished.finish("done")
	s.jobs[1] = running
	s.jobs[2] = finished
	s.next = 3

	all := s.all()
	if len(all) != 2 || all[0].id != 1 || all[1].id != 2 {
		t.Fatalf("all() = %v jobs, want [1 2]", len(all))
	}
	if n := len(s.running()); n != 1 {
		t.Fatalf("running() = %d, want 1", n)
	}
}

// /jobs opens the modal, x cancels the highlighted sub-agent job, esc closes.
func TestJobsModalKeys(t *testing.T) {
	m := typeKeys(t, newSized(t), "/jobs")
	m = press(t, m, tea.KeyEnter, 0)
	if m.state != stateJobs {
		t.Fatalf("state %v, want stateJobs", m.state)
	}

	cancelled := false
	m.ensureSubagentRuntime()
	m.subagents.jobs[1] = &subagentJob{id: 1, tasks: []string{"t"}, started: time.Now(), cancel: func() { cancelled = true }}
	m.subagents.next = 2

	m = typeKeys(t, m, "x")
	if !cancelled {
		t.Fatal("x should cancel the highlighted sub-agent job")
	}
	if !strings.Contains(m.toast, "cancelled") {
		t.Fatalf("toast %q should confirm the cancel", m.toast)
	}

	m = press(t, m, tea.KeyEscape, 0)
	if m.state != stateChat {
		t.Fatalf("esc left state at %v, want stateChat", m.state)
	}
}

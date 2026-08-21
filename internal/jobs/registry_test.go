package jobs

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Ids come from ONE counter shared by every kind, so a shell job and a
// sub-agent job can never collide.
func TestIDsAreUnifiedAcrossKinds(t *testing.T) {
	r := New()
	a := r.Register(KindShell, "sleep 1", Hooks{})
	b := r.Register(KindSubagent, "investigate foo", Hooks{})
	c := r.Register(KindShell, "make build", Hooks{})

	if a.ID() != 1 || b.ID() != 2 || c.ID() != 3 {
		t.Fatalf("expected sequential ids 1,2,3, got %d,%d,%d", a.ID(), b.ID(), c.ID())
	}
	list := r.List()
	if len(list) != 3 || list[0] != a || list[1] != b || list[2] != c {
		t.Fatalf("List should return the jobs in id order, got %v", list)
	}
	if got, ok := r.Get(b.ID()); !ok || got != b {
		t.Fatal("Get did not return the registered job")
	}
	if _, ok := r.Get(99); ok {
		t.Fatal("Get returned an unknown id")
	}
}

func TestStatusTransitions(t *testing.T) {
	r := New()

	done := r.Register(KindShell, "ok", Hooks{})
	if done.State() != StatusRunning {
		t.Fatal("a fresh job must be running")
	}
	done.Finish("exit 0")
	if done.State() != StatusDone || done.Detail() != "exit 0" {
		t.Fatalf("expected done (exit 0), got %s (%s)", done.State(), done.Detail())
	}

	failed := r.Register(KindShell, "boom", Hooks{})
	failed.Fail("exit 3")
	if failed.State() != StatusFailed || failed.Detail() != "exit 3" {
		t.Fatalf("expected failed (exit 3), got %s (%s)", failed.State(), failed.Detail())
	}

	// First settle wins: a late duplicate report must not flip the state.
	failed.Finish("exit 0")
	if failed.State() != StatusFailed {
		t.Fatal("a second settle overwrote the terminal state")
	}
}

// A cancel requested through the registry rewrites the producer's terminal
// report to killed, whatever the producer thought the outcome was.
func TestCancelRewritesSettleToKilled(t *testing.T) {
	r := New()
	cancelled := false
	j := r.Register(KindSubagent, "long task", Hooks{Cancel: func() { cancelled = true }})

	got, err := r.Cancel(j.ID())
	if err != nil || got != j {
		t.Fatalf("Cancel failed: job=%v err=%v", got, err)
	}
	if !cancelled {
		t.Fatal("producer cancel hook was not invoked")
	}
	if j.State() != StatusRunning {
		t.Fatal("cancel is a request: the job stays running until the producer settles it")
	}
	j.Finish("cancelled")
	if j.State() != StatusKilled {
		t.Fatalf("expected killed after cancel+settle, got %s", j.State())
	}

	// Cancelling a terminal job is a no-op that reports the current state.
	if _, err := r.Cancel(j.ID()); err != nil {
		t.Fatalf("cancelling a finished job should not error, got %v", err)
	}
	if j.State() != StatusKilled {
		t.Fatal("a late cancel changed the terminal state")
	}
}

func TestCancelUnsupportedAndUnknown(t *testing.T) {
	r := New()
	j := r.Register(KindShell, "unkillable", Hooks{})
	if _, err := r.Cancel(j.ID()); !errors.Is(err, ErrCancelUnsupported) {
		t.Fatalf("expected ErrCancelUnsupported, got %v", err)
	}
	if _, err := r.Cancel(42); err == nil {
		t.Fatal("expected an error for an unknown id")
	}
}

// Hooks supply the model-facing status and output; without them the record
// falls back to the lifecycle state.
func TestStatusLineAndOutputHooks(t *testing.T) {
	r := New()
	live := true
	j := r.Register(KindShell, "cmd", Hooks{
		Status: func() string {
			if live {
				return "running (pid 1, 2s elapsed)"
			}
			return "exited 0"
		},
		Output: func() string { return "partial output" },
	})
	if j.StatusLine() != "running (pid 1, 2s elapsed)" || j.Output() != "partial output" {
		t.Fatalf("hooks not used: %q / %q", j.StatusLine(), j.Output())
	}
	live = false
	j.Finish("exit 0")
	if j.StatusLine() != "exited 0" {
		t.Fatalf("expected the producer status after settle, got %q", j.StatusLine())
	}

	bare := r.Register(KindSubagent, "report only", Hooks{})
	if bare.StatusLine() != "running" || bare.Output() != "" {
		t.Fatalf("bad fallbacks: %q / %q", bare.StatusLine(), bare.Output())
	}
	bare.Fail("timed out")
	if bare.StatusLine() != "failed (timed out)" {
		t.Fatalf("expected state+detail fallback, got %q", bare.StatusLine())
	}
}

// The registry is shared by producers on detached goroutines and readers on
// the update loop; run with -race.
func TestConcurrentAccess(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			kind := KindShell
			if i%2 == 1 {
				kind = KindSubagent
			}
			j := r.Register(kind, fmt.Sprintf("job-%d", i), Hooks{
				Output: func() string { return "out" },
				Cancel: func() {},
			})
			_, _ = r.Get(j.ID())
			_ = r.List()
			if i%3 == 0 {
				_, _ = r.Cancel(j.ID())
			}
			j.Finish("done")
			_ = j.StatusLine()
			_ = j.Output()
		}(i)
	}
	wg.Wait()
	if got := len(r.List()); got != 32 {
		t.Fatalf("expected 32 jobs, got %d", got)
	}
	seen := map[int]bool{}
	for _, j := range r.List() {
		if seen[j.ID()] {
			t.Fatalf("duplicate id %d", j.ID())
		}
		seen[j.ID()] = true
		if j.State() == StatusRunning {
			t.Fatalf("job %d never settled", j.ID())
		}
	}
}

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// freshnessRegistry builds the minimal registry the freshness tests need:
// the observing read tools and the mutating tools the guard gates.
func freshnessRegistry() *Registry {
	r := NewRegistry()
	r.Register(ReadFileTool())
	r.Register(WriteFileTool())
	r.Register(EditFileTool())
	r.Register(AppendFileTool())
	r.Register(DeleteFileTool())
	r.Register(MoveFileTool())
	r.Register(FileInfoTool())
	r.Register(ListDirectoryTool())
	r.Register(GrepTool())
	return r
}

func freshnessCall(t *testing.T, name string, args map[string]any) ToolCall {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return ToolCall{Function: ToolCallFunction{Name: name, Arguments: raw}}
}

func mustInvoke(t *testing.T, r *Registry, ctx context.Context, name string, args map[string]any) string {
	t.Helper()
	out, err := r.Invoke(ctx, freshnessCall(t, name, args))
	if err != nil {
		t.Fatalf("%s failed: %v", name, err)
	}
	return out
}

// The case this guard exists for: the model reads a file, someone else edits
// it, and the model then edits from its stale copy.
func TestFreshnessStaleEditRefused(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("one\n"), 0o644)

	r := freshnessRegistry()
	ctx := WithFreshnessLedger(context.Background(), NewFreshnessLedger())
	mustInvoke(t, r, ctx, "read_file", map[string]any{"path": p})

	// Nothing changed yet: the edit must go through untouched.
	mustInvoke(t, r, ctx, "edit_file", map[string]any{"path": p, "old_string": "one", "new_string": "two"})

	// A third party rewrote the file: the next edit from the stale copy is refused.
	os.WriteFile(p, []byte("two\n\nuser added this\n"), 0o644)
	_, err := r.Invoke(ctx, freshnessCall(t, "edit_file", map[string]any{"path": p, "old_string": "two", "new_string": "three"}))
	var se *StaleFileError
	if !errors.As(err, &se) {
		t.Fatalf("stale edit was allowed (err=%v)", err)
	}
	if !strings.Contains(err.Error(), "changed on disk") || !strings.Contains(err.Error(), "edit_file") {
		t.Fatalf("unhelpful refusal: %v", err)
	}
}

// A model that ignores the refusal must not be refused forever with the same
// text; re-reading is what clears it, and the guard says so once.
func TestFreshnessRefusalNotSticky(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("one\n"), 0o644)

	r := freshnessRegistry()
	ctx := WithFreshnessLedger(context.Background(), NewFreshnessLedger())
	mustInvoke(t, r, ctx, "read_file", map[string]any{"path": p})
	os.WriteFile(p, []byte("two\n"), 0o644)

	if _, err := r.Invoke(ctx, freshnessCall(t, "write_file", map[string]any{"path": p, "content": "three\n"})); err == nil {
		t.Fatal("expected the first call to be refused")
	}
	// Baseline dropped on refusal: the retry is allowed through (the model was
	// told to re-read; the guard does not nag twice with identical text).
	mustInvoke(t, r, ctx, "write_file", map[string]any{"path": p, "content": "three\n"})
}

// The model's own successful write is not third-party drift.
func TestFreshnessOwnWriteIsNotStale(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("one\n"), 0o644)

	r := freshnessRegistry()
	ctx := WithFreshnessLedger(context.Background(), NewFreshnessLedger())
	mustInvoke(t, r, ctx, "read_file", map[string]any{"path": p})
	mustInvoke(t, r, ctx, "edit_file", map[string]any{"path": p, "old_string": "one", "new_string": "two"})
	// The follow-up edit works against what the model's own edit left behind.
	mustInvoke(t, r, ctx, "edit_file", map[string]any{"path": p, "old_string": "two", "new_string": "three"})
}

// A file the model never observed is not this guard's business — the plan-mode
// read-before-edit gate owns that question, and gating here would block every
// first write in a session.
func TestFreshnessUnreadFileAllowed(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "new.txt")
	os.WriteFile(p, []byte("x\n"), 0o644)

	r := freshnessRegistry()
	ctx := WithFreshnessLedger(context.Background(), NewFreshnessLedger())
	mustInvoke(t, r, ctx, "write_file", map[string]any{"path": p, "content": "y\n"})
}

// Creating a file that does not exist needs no read first — and having created
// it counts as observing it, so a later external change IS drift.
func TestFreshnessNewFileCreationAllowed(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "created.txt")

	r := freshnessRegistry()
	ctx := WithFreshnessLedger(context.Background(), NewFreshnessLedger())
	mustInvoke(t, r, ctx, "write_file", map[string]any{"path": p, "content": "mine\n"})

	os.WriteFile(p, []byte("someone else\n"), 0o644)
	if _, err := r.Invoke(ctx, freshnessCall(t, "edit_file", map[string]any{"path": p, "old_string": "mine", "new_string": "ours"})); err == nil {
		t.Fatal("drift after agent-created file was allowed")
	}
}

// Path spelling must not defeat the ledger: the read and the edit can name the
// same file differently.
func TestFreshnessNormalizesPaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("one\n"), 0o644)

	r := freshnessRegistry()
	ctx := WithFreshnessLedger(context.Background(), NewFreshnessLedger())
	mustInvoke(t, r, ctx, "read_file", map[string]any{"path": p})
	os.WriteFile(p, []byte("two\n"), 0o644)

	spelled := filepath.Join(dir, ".", "a.txt")
	if _, err := r.Invoke(ctx, freshnessCall(t, "edit_file", map[string]any{"path": spelled, "old_string": "one", "new_string": "two"})); err == nil {
		t.Fatal("a differently spelled path evaded the guard")
	}
}

// A deleted file drops its baseline rather than comparing a recreated file
// against a hash for bytes that are gone.
func TestFreshnessDeletedFileDropsBaseline(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("one\n"), 0o644)

	r := freshnessRegistry()
	ctx := WithFreshnessLedger(context.Background(), NewFreshnessLedger())
	mustInvoke(t, r, ctx, "read_file", map[string]any{"path": p})
	mustInvoke(t, r, ctx, "delete_file", map[string]any{"path": p})

	os.WriteFile(p, []byte("recreated\n"), 0o644)
	mustInvoke(t, r, ctx, "write_file", map[string]any{"path": p, "content": "ours now\n"})
}

// move_file checks the destination side too: overwriting a file that drifted
// since the model read it is refused.
func TestFreshnessMoveFileChecksDestination(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	os.WriteFile(src, []byte("incoming\n"), 0o644)
	os.WriteFile(dst, []byte("original\n"), 0o644)

	r := freshnessRegistry()
	ctx := WithFreshnessLedger(context.Background(), NewFreshnessLedger())
	mustInvoke(t, r, ctx, "read_file", map[string]any{"path": dst})
	os.WriteFile(dst, []byte("externally rewritten\n"), 0o644)

	if _, err := r.Invoke(ctx, freshnessCall(t, "move_file", map[string]any{"source": src, "destination": dst})); err == nil {
		t.Fatal("move onto a drifted destination was allowed")
	}
}

// Only successful calls touch the ledger: a failed edit is not an observation,
// and its internal read of the file must not seed a baseline either.
func TestFreshnessFailedMutationSeedsNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("one\n"), 0o644)

	r := freshnessRegistry()
	ctx := WithFreshnessLedger(context.Background(), NewFreshnessLedger())
	if _, err := r.Invoke(ctx, freshnessCall(t, "edit_file", map[string]any{"path": p, "old_string": "absent", "new_string": "x"})); err == nil {
		t.Fatal("edit with a missing old_string should have failed")
	}

	os.WriteFile(p, []byte("two\n"), 0o644)
	// Never observed (the failed edit read the file internally — that is not an
	// observation), so the drift guard stays silent.
	mustInvoke(t, r, ctx, "edit_file", map[string]any{"path": p, "old_string": "two", "new_string": "three"})
}

// Whole-file readers anchor a baseline; fragment tools like grep do not — they
// never showed the model the bytes an edit would overwrite.
func TestFreshnessObservationSources(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("one\n"), 0o644)

	r := freshnessRegistry()
	ctx := WithFreshnessLedger(context.Background(), NewFreshnessLedger())

	// grep surfaces matching lines, not the file: no baseline.
	mustInvoke(t, r, ctx, "grep", map[string]any{"pattern": "one", "path": p})
	os.WriteFile(p, []byte("two\n"), 0o644)
	mustInvoke(t, r, ctx, "write_file", map[string]any{"path": p, "content": "three\n"})

	// file_info is a path-keyed read in the TUI ledger's vocabulary: it observes.
	info := filepath.Join(dir, "b.txt")
	os.WriteFile(info, []byte("one\n"), 0o644)
	mustInvoke(t, r, ctx, "file_info", map[string]any{"path": info})
	os.WriteFile(info, []byte("two\n"), 0o644)
	if _, err := r.Invoke(ctx, freshnessCall(t, "write_file", map[string]any{"path": info, "content": "three\n"})); err == nil {
		t.Fatal("drift after file_info was allowed")
	}
}

// Without a ledger on the context the guard is off — the pre-ledger behavior
// of direct invocations and embedders is unchanged.
func TestFreshnessNoLedgerNoGuard(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("one\n"), 0o644)

	r := freshnessRegistry()
	ctx := context.Background()
	mustInvoke(t, r, ctx, "read_file", map[string]any{"path": p})
	os.WriteFile(p, []byte("two\n"), 0o644)
	mustInvoke(t, r, ctx, "edit_file", map[string]any{"path": p, "old_string": "two", "new_string": "three"})
}

// A stale refusal is already model-coaching text; RepairHint must pass it
// through instead of appending argument-repair advice.
func TestFreshnessRepairHintPassthrough(t *testing.T) {
	err := &StaleFileError{Path: "/x/a.go", Tool: "edit_file"}
	got := RepairHint(freshnessCall(t, "edit_file", map[string]any{"path": "/x/a.go"}), err)
	if !strings.HasPrefix(got, "error: ") || !strings.Contains(got, "changed on disk") {
		t.Fatalf("coaching message lost: %q", got)
	}
	if strings.Contains(got, "Check the arguments") {
		t.Fatalf("misdiagnosed as an argument problem: %q", got)
	}
}

// The ledger is shared by every tool goroutine in a parallel batch; run under
// -race.
func TestFreshnessLedgerRace(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("one\n"), 0o644)

	l := NewFreshnessLedger()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.Observe(p)
				l.RecordMutation([]string{p})
				_ = l.CheckMutation("edit_file", []string{p})
				_ = l.CheckMutation("write_file", []string{filepath.Join(dir, fmt.Sprintf("b%d.txt", i))})
			}
		}(i)
	}
	wg.Wait()
}

// Parallel Invokes against one registry and one session ledger, as a TUI batch
// does; run under -race.
func TestFreshnessConcurrentInvokes(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("one\n"), 0o644)

	r := freshnessRegistry()
	ctx := WithFreshnessLedger(context.Background(), NewFreshnessLedger())
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = r.Invoke(ctx, freshnessCall(t, "read_file", map[string]any{"path": p}))
				_, _ = r.Invoke(ctx, freshnessCall(t, "append_file", map[string]any{"path": p, "content": "x"}))
			}
		}()
	}
	wg.Wait()
}

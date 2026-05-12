package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRememberAndRecall(t *testing.T) {
	dir := t.TempDir()
	store, err := New(filepath.Join(dir, "mem.json"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Remember("user prefers Rust 2024", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("currently debugging auth middleware", false); err != nil {
		t.Fatal(err)
	}

	st, lt := store.Recall("")
	if len(lt) != 1 || !strings.Contains(lt[0].Content, "Rust") {
		t.Errorf("expected 1 long-term entry about Rust, got %+v", lt)
	}
	if len(st) != 1 || !strings.Contains(st[0].Content, "auth") {
		t.Errorf("expected 1 short-term entry about auth, got %+v", st)
	}

	st2, lt2 := store.Recall("rust")
	if len(st2) != 0 || len(lt2) != 1 {
		t.Errorf("query 'rust' should match only long-term: got %d short, %d long", len(st2), len(lt2))
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mem.json")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("durable fact", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Remember("ephemeral fact", false); err != nil {
		t.Fatal(err)
	}

	store2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	st, lt := store2.Recall("")
	if len(lt) != 1 {
		t.Errorf("expected 1 long-term after reload, got %d", len(lt))
	}
	if len(st) != 0 {
		t.Errorf("short-term should not persist, got %d", len(st))
	}
}

func TestLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mem.json")
	if err := os.WriteFile(path, []byte(`{"name":"javan","style":"terse"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	_, lt := store.Recall("")
	if len(lt) != 2 {
		t.Fatalf("expected 2 migrated entries, got %d", len(lt))
	}

	store2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	_, lt2 := store2.Recall("")
	if len(lt2) != 2 {
		t.Errorf("expected 2 entries after re-save, got %d", len(lt2))
	}
}

func TestForget(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "mem.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.Remember("alpha", true)
	store.Remember("beta", true)
	store.Remember("alpha-session", false)

	n, err := store.Forget("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected 2 removed, got %d", n)
	}
	st, lt := store.Recall("")
	if len(st) != 0 || len(lt) != 1 || lt[0].Content != "beta" {
		t.Errorf("after forget, expected only 'beta' in long-term, got %+v %+v", st, lt)
	}
}

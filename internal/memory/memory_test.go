package memory

import (
	"fmt"
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

func TestRememberDeduplicatesNormalizedContent(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "mem.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Remember("User prefers concise answers", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Remember("  user   prefers CONCISE answers  ", true)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("duplicate created a new entry: %q != %q", first.ID, second.ID)
	}
	_, entries := store.Recall("")
	if len(entries) != 1 {
		t.Fatalf("expected one stored entry, got %d", len(entries))
	}
}

func TestLongTermSummaryIsBoundedAndKeepsNewest(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "mem.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxInjectedLongTermEntries+8; i++ {
		if _, err := store.Remember(fmt.Sprintf("memory-%02d %s", i, strings.Repeat("x", 80)), true); err != nil {
			t.Fatal(err)
		}
	}
	summary := store.LongTermSummary()
	if len(summary) > maxInjectedLongTermChars+100 {
		t.Fatalf("summary grew beyond its budget: %d bytes", len(summary))
	}
	if strings.Contains(summary, "memory-00") {
		t.Fatal("oldest memory should have been omitted")
	}
	if !strings.Contains(summary, fmt.Sprintf("memory-%02d", maxInjectedLongTermEntries+7)) {
		t.Fatal("newest memory should be retained")
	}
	if !strings.Contains(summary, "Older memories omitted") {
		t.Fatal("bounded summary should tell the model that recall is available")
	}
	_, all := store.Recall("")
	if len(all) != maxInjectedLongTermEntries+8 {
		t.Fatalf("prompt bounding must not delete stored memory; got %d entries", len(all))
	}
}

package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) (*KVStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.json")
	s, err := NewKVStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestSetGetRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	val, ok := s.Get("k")
	if !ok || val != "v" {
		t.Fatalf("got %v ok=%v", val, ok)
	}
	if _, ok := s.Get("missing"); ok {
		t.Fatal("unexpected hit for missing key")
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	s, path := newStore(t)
	if err := s.Set("archive_1", "first"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("notes_predream_backup", map[string]any{"a": 1.0}); err != nil {
		t.Fatal(err)
	}
	s2, err := NewKVStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if val, ok := s2.Get("archive_1"); !ok || val != "first" {
		t.Fatalf("archive_1: got %v ok=%v", val, ok)
	}
	if val, ok := s2.Get("notes_predream_backup"); !ok || val.(map[string]any)["a"] != 1.0 {
		t.Fatalf("backup: got %v ok=%v", val, ok)
	}
}

func TestLegacyMigration(t *testing.T) {
	s, path := newStore(t)
	if err := s.Set("archive_1", "first"); err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-split store: a single JSON document at path, no dir.
	legacy := `{"archive_2":"second","plain":"x"}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path + ".d"); err != nil {
		t.Fatal(err)
	}
	m, err := NewKVStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if val, ok := m.Get("archive_2"); !ok || val != "second" {
		t.Fatalf("archive_2: got %v ok=%v", val, ok)
	}
	if val, ok := m.Get("plain"); !ok || val != "x" {
		t.Fatalf("plain: got %v ok=%v", val, ok)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("legacy file should be removed after migration")
	}
	// Data must now live in the per-key directory and survive a reopen.
	if _, err := os.Stat(filepath.Join(path+".d", "archive_2"+keyFileExt)); err != nil {
		t.Fatal("per-key file missing after migration")
	}
	m2, err := NewKVStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if val, ok := m2.Get("archive_2"); !ok || val != "second" {
		t.Fatalf("reopen: got %v ok=%v", val, ok)
	}
}

func TestSetDoesNotRewriteOtherKeys(t *testing.T) {
	s, path := newStore(t)
	if err := s.Set("a", "alpha"); err != nil {
		t.Fatal(err)
	}
	aFile := filepath.Join(path+".d", "a"+keyFileExt)
	before, err := os.Stat(aFile)
	if err != nil {
		t.Fatal(err)
	}
	// Filesystem timestamps are the observable proof the file was untouched;
	// sleep past any timestamp granularity first.
	time.Sleep(20 * time.Millisecond)
	if err := s.Set("b", "beta"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(aFile)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("Set rewrote an unrelated key file")
	}
}

func TestDelete(t *testing.T) {
	s, path := newStore(t)
	if err := s.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("k"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("k"); ok {
		t.Fatal("key still present after Delete")
	}
	if _, err := os.Stat(filepath.Join(path+".d", "k"+keyFileExt)); !os.IsNotExist(err) {
		t.Fatal("key file still present after Delete")
	}
	// Deleting a key that was never written is not an error.
	if err := s.Delete("never-set"); err != nil {
		t.Fatal(err)
	}
}

func TestGetFullDataDeepCopy(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Set("nested", map[string]any{"inner": "orig"}); err != nil {
		t.Fatal(err)
	}
	full := s.GetFullData()
	full["nested"].(map[string]any)["inner"] = "mutated"
	full["injected"] = true
	val, _ := s.Get("nested")
	if val.(map[string]any)["inner"] != "orig" {
		t.Fatal("mutation through GetFullData leaked into the store")
	}
	if _, ok := s.Get("injected"); ok {
		t.Fatal("key injected through GetFullData leaked into the store")
	}
}

func TestLatestKeyNumericOrdering(t *testing.T) {
	s, _ := newStore(t)
	for _, k := range []string{"archive_9", "archive_10", "archive_2", "archive_abc"} {
		if err := s.Set(k, k); err != nil {
			t.Fatal(err)
		}
	}
	key, ok := s.LatestKey("archive_")
	if !ok || key != "archive_10" {
		t.Fatalf("got %q ok=%v, want archive_10", key, ok)
	}
	if _, ok := s.LatestKey("nope_"); ok {
		t.Fatal("unexpected hit for unused prefix")
	}
}

func TestArchivePruning(t *testing.T) {
	s, path := newStore(t)
	total := maxArchives + 5
	for i := 0; i < total; i++ {
		if err := s.Set(fmt.Sprintf("archive_%d", i), "x"); err != nil {
			t.Fatal(err)
		}
	}
	full := s.GetFullData()
	count := 0
	for k := range full {
		if _, ok := numericSuffix(k, archivePrefix); ok {
			count++
		}
	}
	if count != maxArchives {
		t.Fatalf("kept %d archives, want %d", count, maxArchives)
	}
	// The newest survive; the five oldest are gone from memory and disk.
	for i := 0; i < 5; i++ {
		k := fmt.Sprintf("archive_%d", i)
		if _, ok := s.Get(k); ok {
			t.Fatalf("%s should have been pruned", k)
		}
		if _, err := os.Stat(filepath.Join(path+".d", k+keyFileExt)); !os.IsNotExist(err) {
			t.Fatalf("%s file should have been pruned", k)
		}
	}
	key, ok := s.LatestKey(archivePrefix)
	if !ok || key != fmt.Sprintf("archive_%d", total-1) {
		t.Fatalf("latest: got %q ok=%v", key, ok)
	}
}

func TestKeyEscaping(t *testing.T) {
	s, path := newStore(t)
	key := "weird/key\x00name"
	if err := s.Set(key, "v"); err != nil {
		t.Fatal(err)
	}
	s2, err := NewKVStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if val, ok := s2.Get(key); !ok || val != "v" {
		t.Fatalf("got %v ok=%v", val, ok)
	}
}

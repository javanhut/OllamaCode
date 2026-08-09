package storage

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// archivePrefix namespaces the rolling compaction archives; the suffix is a
// unix timestamp (archive_<unix>). maxArchives bounds how many are kept —
// an archive is written on every compaction, so without pruning the store
// would grow forever.
const (
	archivePrefix = "archive_"
	maxArchives   = 50
)

// keyFileExt suffixes every per-key file in the store directory.
const keyFileExt = ".json"

type KVStore struct {
	mu sync.RWMutex
	// path is the historical single-file location (~/.ollama_code/archive.json).
	// The live layout is per-key files under dir (path + ".d"); path itself is
	// kept only to migrate stores written by older versions.
	path string
	dir  string
	data map[string]any
}

func NewKVStore(path string) (*KVStore, error) {
	s := &KVStore{
		path: path,
		dir:  path + ".d",
		data: make(map[string]any),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load populates the in-memory map. A legacy single-file store at s.path takes
// precedence while it exists: its presence means migration to the per-key
// directory never completed, so it is (re)migrated and then removed. Once the
// legacy file is gone the directory is authoritative.
func (s *KVStore) load() error {
	if _, err := os.Stat(s.path); err == nil {
		file, err := os.ReadFile(s.path)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(file, &s.data); err != nil {
			return err
		}
		return s.migrate()
	}
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, keyFileExt) || strings.HasPrefix(name, ".") {
			continue
		}
		key, err := url.PathUnescape(strings.TrimSuffix(name, keyFileExt))
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			return err
		}
		var val any
		if err := json.Unmarshal(raw, &val); err != nil {
			return err
		}
		s.data[key] = val
	}
	return nil
}

// migrate splits the legacy single-file map (already loaded into s.data) into
// per-key files, then removes the legacy file. The legacy file is deleted only
// after every key is written, so a crash mid-migration just re-runs it on the
// next open.
func (s *KVStore) migrate() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	for k, v := range s.data {
		if err := s.writeKey(k, v); err != nil {
			return err
		}
	}
	return os.Remove(s.path)
}

// writeKey persists a single key atomically — temp file in the same directory,
// then rename — so a crash mid-write can never leave a truncated, unparseable
// key file behind. Per-key writes keep Set at O(key) I/O instead of rewriting
// the whole store on every call.
func (s *KVStore) writeKey(key string, value any) error {
	file, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".kvstore-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: on success the rename has consumed the name and
	// this is a no-op; on any error path it removes the partial temp file.
	defer os.Remove(tmpName)
	if _, err := tmp.Write(file); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp uses 0600; the store historically sat at 0644 and holds
	// conversation archives rather than secrets, so keep it readable as before.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, s.keyPath(key))
}

// keyPath maps a store key to its file. Keys are path-escaped so arbitrary
// key names (including ones containing "/") stay inside the store directory.
func (s *KVStore) keyPath(key string) string {
	return filepath.Join(s.dir, url.PathEscape(key)+keyFileExt)
}

func (s *KVStore) Set(key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	var pruned []string
	if strings.HasPrefix(key, archivePrefix) {
		pruned = s.pruneArchivesLocked()
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	if err := s.writeKey(key, value); err != nil {
		return err
	}
	// Pruned archives are dropped best-effort after the new key is durably
	// written; a leftover file is harmless (the next archive Set retries).
	for _, k := range pruned {
		os.Remove(s.keyPath(k))
	}
	return nil
}

func (s *KVStore) Get(key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	val, ok := s.data[key]
	return val, ok
}

// GetFullData returns a deep copy of the store, so callers can range over and
// mutate the result without aliasing the live internal map. The copy is a
// JSON round-trip: every value is JSON-marshalable by construction (writeKey
// would have failed otherwise), and this matches the on-disk format exactly.
func (s *KVStore) GetFullData() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, err := json.Marshal(s.data)
	if err != nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(s.data))
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// LatestKey returns the key with the given prefix whose numeric suffix is the
// largest — e.g. the newest archive_<unix>. The suffix is parsed, not compared
// as a string: lexical ordering breaks across digit-width boundaries
// ("archive_9999999999" sorts above "archive_10000000000"). Keys whose suffix
// is not a number are ignored.
func (s *KVStore) LatestKey(prefix string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var (
		best  string
		bestN int64
		found bool
	)
	for k := range s.data {
		n, ok := numericSuffix(k, prefix)
		if !ok {
			continue
		}
		if !found || n > bestN {
			best, bestN, found = k, n, true
		}
	}
	return best, found
}

func (s *KVStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	err := os.Remove(s.keyPath(key))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// pruneArchivesLocked drops all but the newest maxArchives archive entries
// (newest by numeric suffix) and returns the dropped keys so the caller can
// remove their files. Callers must hold s.mu.
func (s *KVStore) pruneArchivesLocked() []string {
	type entry struct {
		key string
		n   int64
	}
	var archives []entry
	for k := range s.data {
		if n, ok := numericSuffix(k, archivePrefix); ok {
			archives = append(archives, entry{k, n})
		}
	}
	if len(archives) <= maxArchives {
		return nil
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].n > archives[j].n })
	dropped := make([]string, 0, len(archives)-maxArchives)
	for _, a := range archives[maxArchives:] {
		delete(s.data, a.key)
		dropped = append(dropped, a.key)
	}
	return dropped
}

// numericSuffix parses the integer following prefix in key
// ("archive_42" with prefix "archive_" → 42).
func numericSuffix(key, prefix string) (int64, bool) {
	if !strings.HasPrefix(key, prefix) {
		return 0, false
	}
	n, err := strconv.ParseInt(key[len(prefix):], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

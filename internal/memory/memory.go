// Package memory implements Layla's two-tier memory:
//   - Short-term: in-process, session-scoped. Holds observations the model
//     wants to keep around for the rest of the conversation but doesn't need
//     to outlive the process.
//   - Long-term: persisted to disk. Anything the user explicitly asks to
//     remember, or that the model decides is worth carrying across sessions.
//
// The store is intentionally simple: a flat list of entries per tier. We sort
// at read time and use substring matching for recall.
package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type Store struct {
	mu        sync.RWMutex
	path      string
	longTerm  []Entry
	shortTerm []Entry
}

type fileFormat struct {
	LongTerm []Entry `json:"long_term"`
}

func New(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var ff fileFormat
	if err := json.Unmarshal(data, &ff); err == nil && ff.LongTerm != nil {
		s.longTerm = ff.LongTerm
		return nil
	}
	// Legacy format: flat key/value map written by the old user_memory tool.
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	keys := make([]string, 0, len(legacy))
	for k := range legacy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	now := time.Now()
	for _, k := range keys {
		s.longTerm = append(s.longTerm, Entry{
			ID:        fmt.Sprintf("legacy-%s", k),
			Content:   fmt.Sprintf("%s: %v", k, legacy[k]),
			CreatedAt: now,
		})
	}
	return s.save()
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(fileFormat{LongTerm: s.longTerm}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o644)
}

// Remember stores content. When persist is true the entry goes to long-term
// memory and is written to disk; otherwise it stays in session-only
// short-term memory.
func (s *Store) Remember(content string, persist bool) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := Entry{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		Content:   strings.TrimSpace(content),
		CreatedAt: time.Now(),
	}
	if persist {
		s.longTerm = append(s.longTerm, e)
		if err := s.save(); err != nil {
			return e, err
		}
		return e, nil
	}
	s.shortTerm = append(s.shortTerm, e)
	return e, nil
}

// Recall returns matching entries from each tier. An empty query returns all.
func (s *Store) Recall(query string) (shortTerm, longTerm []Entry) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q := strings.ToLower(strings.TrimSpace(query))
	match := func(e Entry) bool {
		if q == "" {
			return true
		}
		return strings.Contains(strings.ToLower(e.Content), q)
	}
	for _, e := range s.shortTerm {
		if match(e) {
			shortTerm = append(shortTerm, e)
		}
	}
	for _, e := range s.longTerm {
		if match(e) {
			longTerm = append(longTerm, e)
		}
	}
	return
}

// Forget removes any entry whose content matches the query substring (case
// insensitive). Returns the number removed across both tiers.
func (s *Store) Forget(query string) (int, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	keep := s.shortTerm[:0]
	for _, e := range s.shortTerm {
		if strings.Contains(strings.ToLower(e.Content), q) {
			removed++
			continue
		}
		keep = append(keep, e)
	}
	s.shortTerm = keep
	keep2 := s.longTerm[:0]
	for _, e := range s.longTerm {
		if strings.Contains(strings.ToLower(e.Content), q) {
			removed++
			continue
		}
		keep2 = append(keep2, e)
	}
	s.longTerm = keep2
	if removed > 0 {
		if err := s.save(); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// Promote moves a short-term entry to long-term by ID.
func (s *Store) Promote(id string) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.shortTerm {
		if e.ID == id {
			s.longTerm = append(s.longTerm, e)
			s.shortTerm = append(s.shortTerm[:i], s.shortTerm[i+1:]...)
			if err := s.save(); err != nil {
				return e, true, err
			}
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// PromoteAll flushes the entire short-term tier into long-term.
func (s *Store) PromoteAll() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.shortTerm)
	if n == 0 {
		return 0, nil
	}
	s.longTerm = append(s.longTerm, s.shortTerm...)
	s.shortTerm = nil
	return n, s.save()
}

// LongTermSummary returns long-term entries as a markdown list, oldest first.
// Empty string if no entries.
func (s *Store) LongTermSummary() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.longTerm) == 0 {
		return ""
	}
	sorted := make([]Entry, len(s.longTerm))
	copy(sorted, s.longTerm)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
	})
	var b strings.Builder
	for _, e := range sorted {
		b.WriteString("- ")
		b.WriteString(e.Content)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// ShortTermSummary returns short-term entries as a markdown list.
func (s *Store) ShortTermSummary() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.shortTerm) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range s.shortTerm {
		b.WriteString("- ")
		b.WriteString(e.Content)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// FormatEntries renders a list of entries for tool output.
func FormatEntries(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "- [%s] %s\n", e.CreatedAt.Format("2006-01-02 15:04"), e.Content)
	}
	return strings.TrimRight(b.String(), "\n")
}

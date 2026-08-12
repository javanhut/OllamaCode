package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/api"
)

// Todo is one checklist item as the model maintains it via todo_write. The tui
// package keeps its own todoItem type; this is the wire form a saved session
// carries so todos survive a restart.
type Todo struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

type Session struct {
	Name      string        `json:"name"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Model     string        `json:"model"`
	Mode      string        `json:"mode"`
	Notes     string        `json:"notes"`
	Workspace string        `json:"workspace,omitempty"`
	Todos     []Todo        `json:"todos,omitempty"`
	Messages  []api.Message `json:"messages"`
}

// testDirOverride, when non-empty, replaces the user-config session dir. Only
// tests set it (via SetDirForTesting); production code goes through Dir.
var testDirOverride string

// SetDirForTesting points the session store at dir and returns a function that
// restores the previous location. Not safe for parallel tests.
func SetDirForTesting(dir string) func() {
	prev := testDirOverride
	testDirOverride = dir
	return func() { testDirOverride = prev }
}

// Dir is the directory named sessions are stored in.
func Dir() string {
	if testDirOverride != "" {
		return testDirOverride
	}
	return defaultDir()
}

func defaultDir() string {
	dir, _ := os.UserConfigDir()
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "ollama_code", "sessions")
}

func Save(s Session) error {
	return SaveTo(filepath.Join(Dir(), safeName(s.Name)+".json"), s)
}

// SaveTo writes s to path atomically — temp file in the same directory, then
// rename — so a crash mid-write never leaves a truncated session behind.
func SaveTo(path string, s Session) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".session-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// On success the rename consumes the name and this is a no-op; on any error
	// path it removes the partial temp file.
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func Load(name string) (*Session, error) {
	return LoadFrom(filepath.Join(Dir(), safeName(name)+".json"))
}

func LoadFrom(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func List() ([]Session, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sessions []Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		s, err := Load(name)
		if err != nil {
			continue
		}
		sessions = append(sessions, *s)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
	})
	return sessions, nil
}

func safeName(name string) string {
	return strings.ReplaceAll(name, "/", "_")
}

func Delete(name string) error {
	path := filepath.Join(Dir(), safeName(name)+".json")
	return os.Remove(path)
}

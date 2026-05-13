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

type Session struct {
	Name      string        `json:"name"`
	CreatedAt time.Time     `json:"created_at"`
	Model     string        `json:"model"`
	Mode      string        `json:"mode"`
	Notes     string        `json:"notes"`
	Messages  []api.Message `json:"messages"`
}

func defaultDir() string {
	dir, _ := os.UserConfigDir()
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "ollama_code", "sessions")
}

func Save(s Session) error {
	dir := defaultDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, safeName(s.Name)+".json")
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func Load(name string) (*Session, error) {
	path := filepath.Join(defaultDir(), safeName(name)+".json")
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
	entries, err := os.ReadDir(defaultDir())
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
	path := filepath.Join(defaultDir(), safeName(name)+".json")
	return os.Remove(path)
}

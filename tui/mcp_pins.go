package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/javanhut/ollama_code/tools"
)

// MCP tool-definition pinning. trusted=true approves a server as configured,
// but a server's tool list can change after approval — a new tool, or an old
// one whose description now carries injected instructions (the "rug pull":
// Cursor's MCPoison bound trust to the entry name and was defeated exactly
// this way). So the first successful connection records a hash of every tool's
// name, description and input schema, and any later mismatch disables the
// server's tools until the user reviews it and runs /mcp repin <name>.
//
// Trust-on-first-use: the user's trusted=true is the approval of what the
// server offers at that moment. The pin file lives beside config.json, outside
// any workspace, so the model's sandboxed shell cannot rewrite it.

type mcpPinStore struct {
	mu   sync.Mutex
	path string
	pins map[string]string
}

func mcpPinsPath() string {
	if p := configPath(); p != "" {
		return filepath.Join(filepath.Dir(p), "mcp_pins.json")
	}
	return ""
}

func loadMCPPins(path string) *mcpPinStore {
	s := &mcpPinStore{path: path, pins: map[string]string{}}
	if path == "" {
		return s
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.pins)
	}
	return s
}

func (s *mcpPinStore) get(server string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.pins[server]
	return h, ok
}

func (s *mcpPinStore) set(server, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if hash == "" {
		delete(s.pins, server)
	} else {
		s.pins[server] = hash
	}
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.pins, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

// mcpToolsHash is order-independent: servers may list tools in any order.
func mcpToolsHash(defs []tools.Tool) string {
	type entry struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	entries := make([]entry, 0, len(defs))
	for _, d := range defs {
		params, _ := json.Marshal(d.Function.Parameters)
		entries = append(entries, entry{d.Function.Name, d.Function.Description, params})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	data, _ := json.Marshal(entries)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// checkMCPPin compares defs against the stored pin, recording one on first
// use. ok=false means the definitions changed since they were approved.
func checkMCPPin(pins *mcpPinStore, server string, defs []tools.Tool) (ok bool, err error) {
	if pins == nil {
		return true, nil
	}
	hash := mcpToolsHash(defs)
	pinned, exists := pins.get(server)
	if !exists {
		return true, pins.set(server, hash)
	}
	return pinned == hash, nil
}

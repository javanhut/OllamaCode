package lsp

import (
	"os"
	"path/filepath"
	"strings"
)

// Server declares how to launch one language server and what it answers for.
// The built-in table stays deliberately short: four servers cover the languages
// ocode's own verification arms already know, and everything beyond that is a
// guess about install paths that cannot be tested here. Users add their own
// through config.json's "lsp_servers", which is the same struct.
type Server struct {
	Name string `json:"-"`
	// Command is the binary to run; it is looked up on PATH and the server is
	// skipped when absent, so an uninstalled server costs nothing.
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	// Extensions are lowercased file extensions including the dot.
	Extensions []string `json:"extensions"`
	// RootMarkers are files that identify the project root. The first marker
	// found walking up from a queried file wins; with none found the workspace
	// root is used.
	RootMarkers []string `json:"root_markers,omitempty"`
	// LanguageID is the protocol's language identifier sent on didOpen. Empty
	// means derive it from the extension.
	LanguageID string `json:"language_id,omitempty"`
}

// Builtins are the servers ocode knows without configuration.
func Builtins() []Server {
	return []Server{
		{
			Name:        "gopls",
			Command:     "gopls",
			Extensions:  []string{".go"},
			RootMarkers: []string{"go.mod", "go.work"},
			LanguageID:  "go",
		},
		{
			Name:        "pyright",
			Command:     "pyright-langserver",
			Args:        []string{"--stdio"},
			Extensions:  []string{".py", ".pyi"},
			RootMarkers: []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt"},
			LanguageID:  "python",
		},
		{
			Name:        "typescript",
			Command:     "typescript-language-server",
			Args:        []string{"--stdio"},
			Extensions:  []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"},
			RootMarkers: []string{"tsconfig.json", "jsconfig.json", "package.json"},
		},
		{
			Name:        "rust-analyzer",
			Command:     "rust-analyzer",
			Extensions:  []string{".rs"},
			RootMarkers: []string{"Cargo.toml"},
			LanguageID:  "rust",
		},
	}
}

// handles reports whether this server claims a file.
func (s Server) handles(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, candidate := range s.Extensions {
		if strings.ToLower(candidate) == ext {
			return true
		}
	}
	return false
}

// languageID is the protocol identifier for a file, from the server's explicit
// setting or derived from the extension.
func (s Server) languageID(path string) string {
	if s.LanguageID != "" {
		return s.LanguageID
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".jsx":
		return "javascriptreact"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".pyi":
		return "python"
	default:
		return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	}
}

// rootFor walks up from a file looking for one of the server's root markers,
// stopping at the workspace root. Getting this right matters: launched at the
// wrong root, gopls indexes the wrong module and rust-analyzer indexes nothing.
func (s Server) rootFor(path, workspace string) string {
	if len(s.RootMarkers) == 0 {
		return workspace
	}
	dir := filepath.Dir(path)
	for {
		for _, marker := range s.RootMarkers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(dir, workspace) {
			return workspace
		}
		dir = parent
	}
}

// mergeServers layers user-declared servers over the built-ins, keyed by name,
// so config can both add new servers and replace a built-in whose command is
// wrong on this machine.
func mergeServers(builtin []Server, extra map[string]Server) []Server {
	out := make([]Server, 0, len(builtin)+len(extra))
	replaced := map[string]bool{}
	for _, server := range builtin {
		if override, ok := extra[server.Name]; ok {
			override.Name = server.Name
			out = append(out, override)
			replaced[server.Name] = true
			continue
		}
		out = append(out, server)
	}
	for name, server := range extra {
		if replaced[name] {
			continue
		}
		server.Name = name
		out = append(out, server)
	}
	return out
}

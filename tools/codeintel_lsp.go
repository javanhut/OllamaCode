package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/javanhut/ollama_code/internal/lsp"
)

// The LSP tier sits above the tree-sitter and grep tiers in the code_* tools.
// It answers with a real compiler's index when a language server is installed
// for the file's language, and declines — exactly like the tree-sitter stubs —
// whenever it is disabled, has no server, or the server does not answer in
// time. Every caller below it still works, so this tier is pure upside.
//
// Servers are started on first use, not at boot: a session that never asks a
// code intelligence question never pays for gopls.

// maxLSPResults caps how many locations reach the model. A references query on
// a popular helper can return thousands; the tail is never the useful part.
const maxLSPResults = 50

var (
	lspMu        sync.Mutex
	lspWorkspace string
	lspExtra     map[string]lsp.Server
	lspDisabled  bool
	lspManager   *lsp.Manager
)

// ConfigureLSP records the workspace and any user-declared servers, and
// enables or disables the tier. Called once from startup, before any tool runs.
// Passing enabled=false leaves every LSP path inert.
func ConfigureLSP(enabled bool, workspace string, extra map[string]lsp.Server) {
	lspMu.Lock()
	defer lspMu.Unlock()
	if lspManager != nil {
		go lspManager.Close()
		lspManager = nil
	}
	lspDisabled = !enabled
	lspWorkspace = workspace
	lspExtra = extra
}

// CloseLSP shuts down every running language server. Wired to session exit;
// without it the servers outlive the process that started them.
func CloseLSP() {
	lspMu.Lock()
	manager := lspManager
	lspManager = nil
	lspMu.Unlock()
	manager.Close()
}

// manager returns the shared manager, building it on first use. nil means the
// tier is disabled.
func manager() *lsp.Manager {
	lspMu.Lock()
	defer lspMu.Unlock()
	if lspDisabled {
		return nil
	}
	if lspManager == nil {
		workspace := lspWorkspace
		if workspace == "" {
			workspace = "."
		}
		lspManager = lsp.NewManager(workspace, lspExtra)
	}
	return lspManager
}

// lspDefinitionAt returns grep-shaped `path:line:col` lines for the definition
// of the symbol at a one-based position, or ok=false to fall through.
func lspDefinitionAt(ctx context.Context, path string, line, col int) (string, bool) {
	m := manager()
	if m == nil {
		return "", false
	}
	locs := m.Definition(ctx, path, line, col)
	if len(locs) == 0 {
		return "", false
	}
	return strings.Join(lsp.LocationLines(locs, maxLSPResults), "\n"), true
}

// lspReferencesAt is the same for every use of the symbol.
func lspReferencesAt(ctx context.Context, path string, line, col int) (string, bool) {
	m := manager()
	if m == nil {
		return "", false
	}
	locs := m.References(ctx, path, line, col)
	if len(locs) == 0 {
		return "", false
	}
	lines := lsp.LocationLines(locs, maxLSPResults)
	out := strings.Join(lines, "\n")
	if len(locs) > len(lines) {
		out += fmt.Sprintf("\n\n... and %d more (truncated at %d; narrow your search)", len(locs)-len(lines), maxLSPResults)
	}
	return out, true
}

// lspHoverAt returns the server's description of the symbol at a position.
func lspHoverAt(ctx context.Context, path string, line, col int) (string, bool) {
	m := manager()
	if m == nil {
		return "", false
	}
	text := strings.TrimSpace(m.Hover(ctx, path, line, col))
	if text == "" {
		return "", false
	}
	return text, true
}

// LSPDiagnostics returns the language servers' errors and warnings for the
// given files as capped `path:line:col: message` lines, or "" when no server
// applies. Informational only, on the same contract as verification.Lint: a
// missing server is silence, never a failure, so pre-existing findings can
// never trap the model in a repair loop over code it did not touch.
func LSPDiagnostics(ctx context.Context, paths []string) string {
	m := manager()
	if m == nil || len(paths) == 0 {
		return ""
	}
	diags := m.Diagnostics(ctx, paths)
	if len(diags) == 0 {
		return ""
	}
	extra := 0
	if len(diags) > maxLSPResults {
		extra = len(diags) - maxLSPResults
		diags = diags[:maxLSPResults]
	}
	out := strings.Join(diags, "\n")
	if extra > 0 {
		out += fmt.Sprintf("\n… (%d more diagnostics omitted)", extra)
	}
	return out
}

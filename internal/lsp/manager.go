package lsp

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// initializeTimeout is how long a server gets to hand back capabilities.
	// gopls and rust-analyzer index on startup, so this is generous — but it is
	// paid once per server, not per query.
	initializeTimeout = 20 * time.Second
	// queryTimeout bounds one definition/references/hover round trip.
	queryTimeout = 10 * time.Second
	// diagnosticsWait is how long to wait for a server to publish diagnostics
	// after a file is opened or changed. Diagnostics arrive as an unsolicited
	// notification with no reply to wait on, so this is a poll with a ceiling.
	diagnosticsWait = 3 * time.Second
	diagnosticsPoll = 50 * time.Millisecond
)

// Manager owns the running language servers for one workspace. It is safe for
// concurrent use, and every method answers "nothing" rather than failing when a
// server is missing, broken, or slow: callers all have a fallback path, and a
// language server is an optimization, never a dependency.
type Manager struct {
	workspace string
	servers   []Server

	mu      sync.Mutex
	clients map[string]*Client // key: server name + "\x00" + root
	failed  map[string]bool    // servers that failed to start; not retried
	opened  map[string]int     // client key + "\x00" + uri -> document version
}

// NewManager builds a manager for a workspace. extra comes from config and may
// override a built-in by name or add a new server.
func NewManager(workspace string, extra map[string]Server) *Manager {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		abs = workspace
	}
	servers := mergeServers(Builtins(), extra)
	// Deterministic order so that two servers claiming one extension resolve
	// the same way on every run rather than per map iteration.
	sort.SliceStable(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	return &Manager{
		workspace: abs,
		servers:   servers,
		clients:   map[string]*Client{},
		failed:    map[string]bool{},
		opened:    map[string]int{},
	}
}

// Close shuts down every running server.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	clients := m.clients
	m.clients = map[string]*Client{}
	m.opened = map[string]int{}
	m.mu.Unlock()
	for _, client := range clients {
		client.Close()
	}
}

// serverFor picks the server that claims a path, or ok=false when none does or
// its binary is not installed.
func (m *Manager) serverFor(path string) (Server, bool) {
	for _, server := range m.servers {
		if !server.handles(path) {
			continue
		}
		if _, err := exec.LookPath(server.Command); err != nil {
			continue
		}
		return server, true
	}
	return Server{}, false
}

// clientFor returns a started, initialized client for a path, launching one on
// first use. A server that fails to start is remembered as failed so the next
// query does not pay the same timeout again.
func (m *Manager) clientFor(ctx context.Context, path string) (*Client, Server, bool) {
	server, ok := m.serverFor(path)
	if !ok {
		return nil, Server{}, false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, Server{}, false
	}
	root := server.rootFor(abs, m.workspace)
	key := server.Name + "\x00" + root

	m.mu.Lock()
	if m.failed[key] {
		m.mu.Unlock()
		return nil, Server{}, false
	}
	if client, ok := m.clients[key]; ok {
		m.mu.Unlock()
		return client, server, true
	}
	m.mu.Unlock()

	client, err := m.start(ctx, server, root)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.failed[key] = true
		return nil, Server{}, false
	}
	// Another query may have started the same server while this one was
	// initializing; keep the winner and discard the duplicate process.
	if existing, ok := m.clients[key]; ok {
		go client.Close()
		return existing, server, true
	}
	m.clients[key] = client
	return client, server, true
}

func (m *Manager) start(ctx context.Context, server Server, root string) (*Client, error) {
	client, err := dial(ctx, server.Command, server.Args, root)
	if err != nil {
		return nil, err
	}
	initCtx, cancel := context.WithTimeout(ctx, initializeTimeout)
	defer cancel()
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   pathToURI(root),
		"workspaceFolders": []map[string]string{
			{"uri": pathToURI(root), "name": filepath.Base(root)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization": map[string]any{"didSave": false, "dynamicRegistration": false},
				"definition":      map[string]any{"linkSupport": true},
				"references":      map[string]any{"dynamicRegistration": false},
				"hover":           map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
				"publishDiagnostics": map[string]any{
					"relatedInformation": false,
				},
			},
		},
	}
	if _, err := client.call(initCtx, "initialize", params); err != nil {
		client.Close()
		return nil, err
	}
	if err := client.notify("initialized", map[string]any{}); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

// sync opens a document, or reports its current contents when already open.
// Full-text sync is used throughout: files here are source files a human wrote,
// and incremental sync would be a lot of bookkeeping to save a few kilobytes.
func (m *Manager) sync(client *Client, server Server, path string) (uri string, ok bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", false
	}
	uri = pathToURI(abs)
	key := server.Name + "\x00" + uri

	m.mu.Lock()
	version, alreadyOpen := m.opened[key]
	version++
	m.opened[key] = version
	m.mu.Unlock()

	if alreadyOpen {
		err = client.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": version},
			"contentChanges": []map[string]any{{"text": string(data)}},
		})
	} else {
		err = client.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri":        uri,
				"languageId": server.languageID(abs),
				"version":    version,
				"text":       string(data),
			},
		})
	}
	return uri, err == nil
}

// position builds the protocol's zero-based position from the one-based line
// and column the tools layer uses.
func position(line, col int) Position {
	if line < 1 {
		line = 1
	}
	if col < 1 {
		col = 1
	}
	return Position{Line: line - 1, Character: col - 1}
}

// Definition returns the locations where the symbol at a one-based line/column
// is defined.
func (m *Manager) Definition(ctx context.Context, path string, line, col int) []Location {
	return m.locate(ctx, "textDocument/definition", path, line, col, nil)
}

// References returns every use of the symbol at a one-based line/column,
// including its declaration.
func (m *Manager) References(ctx context.Context, path string, line, col int) []Location {
	return m.locate(ctx, "textDocument/references", path, line, col, map[string]any{
		"context": map[string]any{"includeDeclaration": true},
	})
}

func (m *Manager) locate(ctx context.Context, method, path string, line, col int, extra map[string]any) []Location {
	client, server, ok := m.clientFor(ctx, path)
	if !ok {
		return nil
	}
	uri, ok := m.sync(client, server, path)
	if !ok {
		return nil
	}
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     position(line, col),
	}
	maps.Copy(params, extra)
	callCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	raw, err := client.call(callCtx, method, params)
	if err != nil {
		return nil
	}
	return decodeLocations(raw)
}

// Hover returns the server's description of the symbol at a position, already
// flattened to text.
func (m *Manager) Hover(ctx context.Context, path string, line, col int) string {
	client, server, ok := m.clientFor(ctx, path)
	if !ok {
		return ""
	}
	uri, ok := m.sync(client, server, path)
	if !ok {
		return ""
	}
	callCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	raw, err := client.call(callCtx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     position(line, col),
	})
	if err != nil {
		return ""
	}
	return decodeHover(raw)
}

// Diagnostics returns the errors and warnings a server reports for the given
// files, as "path:line:col: message" lines. Files with no server, or whose
// server publishes nothing before the wait expires, contribute nothing — an
// absent diagnostic never means "clean" to the caller, which is why this is
// only ever used to sharpen a repair message, not to decide pass/fail.
func (m *Manager) Diagnostics(ctx context.Context, paths []string) []string {
	var out []string
	for _, path := range paths {
		client, server, ok := m.clientFor(ctx, path)
		if !ok {
			continue
		}
		uri, ok := m.sync(client, server, path)
		if !ok {
			continue
		}
		for _, d := range m.awaitDiagnostics(ctx, client, uri) {
			if d.Severity > SeverityWarning && d.Severity != 0 {
				continue // information and hints are noise in a repair message
			}
			out = append(out, formatDiagnostic(path, d))
		}
	}
	sort.Strings(out)
	return out
}

// awaitDiagnostics polls for a publish covering uri. The protocol gives no
// reply to wait on, so a bounded poll is the honest implementation.
func (m *Manager) awaitDiagnostics(ctx context.Context, client *Client, uri string) []Diagnostic {
	deadline := time.Now().Add(diagnosticsWait)
	for {
		if diags, ok := client.diagnosticsFor(uri); ok {
			return diags
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-client.done:
			return nil
		case <-time.After(diagnosticsPoll):
		}
	}
}

func formatDiagnostic(path string, d Diagnostic) string {
	var b strings.Builder
	b.WriteString(path)
	b.WriteString(":")
	b.WriteString(strconv.Itoa(d.Range.Start.Line + 1))
	b.WriteString(":")
	b.WriteString(strconv.Itoa(d.Range.Start.Character + 1))
	b.WriteString(": ")
	if d.Severity == SeverityWarning {
		b.WriteString("warning: ")
	}
	b.WriteString(strings.TrimSpace(d.Message))
	if code := codeString(d.Code); code != "" {
		b.WriteString(" (")
		b.WriteString(code)
		b.WriteString(")")
	}
	return b.String()
}

func codeString(code any) string {
	switch v := code.(type) {
	case string:
		return v
	case float64:
		return strconv.Itoa(int(v))
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

// LocationLines renders locations as "path:line:col" for the tools layer, which
// speaks grep output.
func LocationLines(locs []Location, limit int) []string {
	var out []string
	for i, loc := range locs {
		if limit > 0 && i >= limit {
			break
		}
		out = append(out, uriToPath(loc.URI)+":"+strconv.Itoa(loc.Range.Start.Line+1)+":"+strconv.Itoa(loc.Range.Start.Character+1))
	}
	return out
}

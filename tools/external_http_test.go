package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMCPHTTP is a minimal MCP Streamable HTTP server for transport tests.
type fakeMCPHTTP struct {
	t *testing.T

	mu            sync.Mutex
	headers       []http.Header
	sessionIDs    []string
	deleteSeen    bool
	oversizeReply bool
}

func (f *fakeMCPHTTP) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.headers = append(f.headers, r.Header.Clone())
	f.sessionIDs = append(f.sessionIDs, r.Header.Get("Mcp-Session-Id"))
	oversize := f.oversizeReply
	f.mu.Unlock()

	w.Header().Set("Mcp-Session-Id", "test-session-1")
	switch r.Method {
	case http.MethodGet:
		// No server-initiated notifications in the fixture; hang until Close
		// cancels the event stream.
		<-r.Context().Done()
		return
	case http.MethodDelete:
		f.mu.Lock()
		f.deleteSeen = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodPost:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == 0 { // notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "fixture", "version": "1"},
		}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{
			"name": "echo", "description": "Echo text.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
			},
		}}}
	case "tools/call":
		var params struct {
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &params)
		text := "echo: " + fmt.Sprint(params.Arguments["text"])
		if oversize {
			text = strings.Repeat("x", 8*1024)
		}
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	default:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"unknown method"}}`, req.ID)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	_, _ = w.Write(response)
}

func newFakeMCPHTTPServer(t *testing.T) (*httptest.Server, *fakeMCPHTTP) {
	t.Helper()
	fake := &fakeMCPHTTP{t: t}
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(server.Close)
	return server, fake
}

func (f *fakeMCPHTTP) lastHeader(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range slices.Backward(f.headers) {
		if value := v.Get(name); value != "" {
			return value
		}
	}
	return ""
}

func (f *fakeMCPHTTP) sawDelete() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleteSeen
}

// sessionIDOn reports the Mcp-Session-Id observed for the given JSON-RPC method.
func (f *fakeMCPHTTP) sessionOnToolsList() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Requests arrive in order: initialize, initialized notification, tools/list.
	for i, header := range f.headers {
		if header.Get("Content-Type") == "application/json" && i < len(f.sessionIDs) && f.sessionIDs[i] != "" {
			return f.sessionIDs[i]
		}
	}
	return ""
}

func TestHTTPExternalServerLifecycleAndToolCall(t *testing.T) {
	ts, fake := newFakeMCPHTTPServer(t)
	server, err := NewHTTPExternalServer(HTTPExternalServerOptions{Name: "fixture", URL: ts.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Initialize(ctx, "2025-11-25"); err != nil {
		t.Fatal(err)
	}
	policy := ToolPolicy{Modes: ModeReadOnly, SmallModelSafe: true, Network: true, Cost: ToolCostHigh}
	definitions, err := server.ListTools(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 || definitions[0].Function.Name != "mcp_fixture_echo" {
		t.Fatalf("unexpected MCP definitions: %#v", definitions)
	}
	if definitions[0].Policy.Modes != ModeReadOnly || !definitions[0].Policy.SmallModelSafe {
		t.Fatalf("policy not applied to HTTP tools: %#v", definitions[0].Policy)
	}
	got, err := definitions[0].Handler(ctx, json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo: hello" {
		t.Fatalf("unexpected MCP result: %q", got)
	}
	if id := fake.sessionOnToolsList(); id != "test-session-1" {
		t.Fatalf("session id not carried across requests: %q", id)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if !fake.sawDelete() {
		t.Fatal("Close did not DELETE the remote session")
	}
}

func TestHTTPExternalServerSendsConfiguredHeaders(t *testing.T) {
	t.Setenv("MCP_HTTP_TEST_TOKEN", "secret-from-env")
	ts, fake := newFakeMCPHTTPServer(t)
	headers, err := ResolveExternalHeaders(
		map[string]string{"Authorization": "Bearer static", "X-Tenant": "acme"},
		map[string]string{"Authorization": "MCP_HTTP_TEST_TOKEN"},
	)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewHTTPExternalServer(HTTPExternalServerOptions{Name: "fixture", URL: ts.URL, Headers: headers})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Initialize(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if got := fake.lastHeader("Authorization"); got != "secret-from-env" {
		t.Fatalf("env-indirected header not sent (or static won): %q", got)
	}
	if got := fake.lastHeader("X-Tenant"); got != "acme" {
		t.Fatalf("static header not sent: %q", got)
	}
}

func TestResolveExternalHeaders(t *testing.T) {
	t.Setenv("MCP_HTTP_RESOLVE_SET", "resolved")
	t.Setenv("MCP_HTTP_RESOLVE_EMPTY", "")

	merged, err := ResolveExternalHeaders(
		map[string]string{"Authorization": "Bearer fallback"},
		map[string]string{"Authorization": "MCP_HTTP_RESOLVE_UNSET"},
	)
	if err != nil {
		t.Fatalf("static fallback should satisfy an unset env var: %v", err)
	}
	if merged["Authorization"] != "Bearer fallback" {
		t.Fatalf("fallback header lost: %v", merged)
	}

	merged, err = ResolveExternalHeaders(nil, map[string]string{"Authorization": "MCP_HTTP_RESOLVE_SET"})
	if err != nil {
		t.Fatal(err)
	}
	if merged["Authorization"] != "resolved" {
		t.Fatalf("env value not resolved: %v", merged)
	}

	if _, err := ResolveExternalHeaders(nil, map[string]string{"Authorization": "MCP_HTTP_RESOLVE_UNSET"}); err == nil {
		t.Fatal("expected error for unset env var with no fallback")
	}
	if _, err := ResolveExternalHeaders(nil, map[string]string{"Authorization": "MCP_HTTP_RESOLVE_EMPTY"}); err == nil {
		t.Fatal("expected error for empty env var with no fallback")
	}
	if _, err := ResolveExternalHeaders(nil, map[string]string{"": "MCP_HTTP_RESOLVE_SET"}); err == nil {
		t.Fatal("expected error for blank header name")
	}
}

func TestNewMCPServerFromSpecValidation(t *testing.T) {
	if _, err := NewMCPServerFromSpec(ExternalServerSpec{Name: "empty"}); err == nil ||
		!strings.Contains(err.Error(), "either a command") {
		t.Fatalf("expected missing-transport error, got %v", err)
	}
	if _, err := NewMCPServerFromSpec(ExternalServerSpec{Name: "both", Command: "cat", URL: "http://x/mcp"}); err == nil ||
		!strings.Contains(err.Error(), "exactly one transport") {
		t.Fatalf("expected conflicting-transport error, got %v", err)
	}
	httpServer, err := NewMCPServerFromSpec(ExternalServerSpec{Name: "web", URL: "http://localhost:1/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := httpServer.(*HTTPExternalServer); !ok {
		t.Fatalf("url spec produced %T, want *HTTPExternalServer", httpServer)
	}
	if httpServer.Namespace() != "mcp_web_" {
		t.Fatalf("HTTP namespace diverges from stdio namespacing: %q", httpServer.Namespace())
	}
	if _, err := NewMCPServerFromSpec(ExternalServerSpec{
		Name: "badenv", URL: "http://localhost:1/mcp",
		HeadersEnv: map[string]string{"Authorization": "MCP_HTTP_SPEC_UNSET"},
	}); err == nil {
		t.Fatal("expected error for unresolvable headers_env")
	}
}

func TestHTTPExternalServerHonorsMaxResponseBytes(t *testing.T) {
	ts, fake := newFakeMCPHTTPServer(t)
	fake.mu.Lock()
	fake.oversizeReply = true
	fake.mu.Unlock()
	server, err := NewHTTPExternalServer(HTTPExternalServerOptions{
		Name: "fixture", URL: ts.URL, MaxResponseBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// initialize itself is small; tools/call returns 8 KiB > 1 KiB cap.
	if err := server.Initialize(ctx, ""); err != nil {
		t.Fatal(err)
	}
	definitions, err := server.ListTools(ctx, ToolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := definitions[0].Handler(ctx, json.RawMessage(`{"text":"hi"}`)); err == nil ||
		!strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("expected response-size error, got %v", err)
	}
}

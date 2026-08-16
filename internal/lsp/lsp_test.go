package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeServerArg makes the test binary re-execute itself as a language server:
// the client speaks to a real process over real pipes, so framing, request
// correlation, and unsolicited notifications are all exercised for real.
const fakeServerArg = "lsp-fake-server"

func TestFakeLanguageServer(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != fakeServerArg {
		t.Skip("helper process; runs only when re-executed by TestClientEndToEnd")
	}
	serveFake()
	os.Exit(0)
}

// serveFake answers the handful of methods the manager sends.
func serveFake() {
	br := bufio.NewReader(os.Stdin)
	send := func(v any) {
		body, _ := json.Marshal(v)
		fmt.Printf("Content-Length: %d\r\n\r\n%s", len(body), body)
		os.Stdout.Sync()
	}
	for {
		length, err := readHeader(br)
		if err != nil {
			return
		}
		body := make([]byte, length)
		if _, err := readFull(br, body); err != nil {
			return
		}
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &msg); err != nil {
			return
		}
		switch msg.Method {
		case "exit":
			return
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": map[string]any{"capabilities": map[string]any{}}})
		case "textDocument/didOpen":
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			send(map[string]any{
				"jsonrpc": "2.0",
				"method":  "textDocument/publishDiagnostics",
				"params": map[string]any{
					"uri": params.TextDocument.URI,
					"diagnostics": []map[string]any{
						{
							"range":    map[string]any{"start": map[string]int{"line": 3, "character": 6}},
							"severity": SeverityError,
							"code":     "E999",
							"message":  "undefined name spam",
						},
						{
							"range":    map[string]any{"start": map[string]int{"line": 0, "character": 0}},
							"severity": 4, // hint: must be filtered out
							"message":  "consider a docstring",
						},
					},
				},
			})
		case "textDocument/definition":
			send(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": []map[string]any{
				{
					"uri":   "file:///tmp/target%20dir/lib.py",
					"range": map[string]any{"start": map[string]int{"line": 41, "character": 4}},
				},
			}})
		case "textDocument/hover":
			send(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": map[string]any{
				"contents": map[string]any{"kind": "markdown", "value": "def spam() -> int"},
			}})
		default:
			if msg.ID != nil {
				send(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": nil})
			}
		}
	}
}

func readFull(br *bufio.Reader, buf []byte) (int, error) {
	read := 0
	for read < len(buf) {
		n, err := br.Read(buf[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

// managerWithFakeServer builds a manager whose only server is this test binary
// acting as a language server for .py files.
func managerWithFakeServer(t *testing.T, workspace string) *Manager {
	t.Helper()
	m := NewManager(workspace, nil)
	m.servers = []Server{{
		Name:       "fake",
		Command:    os.Args[0],
		Args:       []string{"-test.run=TestFakeLanguageServer", "--", fakeServerArg},
		Extensions: []string{".py"},
		LanguageID: "python",
	}}
	t.Cleanup(m.Close)
	return m
}

func TestClientEndToEnd(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "app.py")
	if err := os.WriteFile(file, []byte("def main():\n    return spam()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := managerWithFakeServer(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	locs := m.Definition(ctx, file, 2, 12)
	if len(locs) != 1 {
		t.Fatalf("expected one definition, got %#v", locs)
	}
	if got := LocationLines(locs, 10); len(got) != 1 || got[0] != "/tmp/target dir/lib.py:42:5" {
		// Line and column are converted back to one-based, and the URI is unescaped.
		t.Fatalf("unexpected location line: %#v", got)
	}

	if hover := m.Hover(ctx, file, 2, 12); hover != "def spam() -> int" {
		t.Fatalf("unexpected hover: %q", hover)
	}

	diags := m.Diagnostics(ctx, []string{file})
	if len(diags) != 1 {
		t.Fatalf("expected the hint to be filtered out, got %#v", diags)
	}
	if !strings.HasSuffix(diags[0], "4:7: undefined name spam (E999)") {
		t.Fatalf("unexpected diagnostic: %q", diags[0])
	}
}

// A second query must reuse the already-running server rather than starting a
// new one, or every code_* call would pay gopls's startup cost again.
func TestManagerReusesClient(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "app.py")
	if err := os.WriteFile(file, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := managerWithFakeServer(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	m.Definition(ctx, file, 1, 1)
	m.Definition(ctx, file, 1, 1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.clients) != 1 {
		t.Fatalf("expected one reused client, got %d", len(m.clients))
	}
}

func TestManagerDeclinesWithoutServer(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir()) // no language server installed anywhere
	m := NewManager(dir, nil)
	t.Cleanup(m.Close)
	if locs := m.Definition(context.Background(), file, 1, 1); locs != nil {
		t.Fatalf("expected no answer without an installed server, got %#v", locs)
	}
	if diags := m.Diagnostics(context.Background(), []string{file}); diags != nil {
		t.Fatalf("expected no diagnostics without an installed server, got %#v", diags)
	}
}

// A server that dies during startup must be remembered, not retried on every
// query — otherwise each code_* call pays the full initialize timeout.
func TestManagerRemembersFailedServer(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.py")
	if err := os.WriteFile(file, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir, nil)
	m.servers = []Server{{Name: "broken", Command: "false", Extensions: []string{".py"}}}
	t.Cleanup(m.Close)
	if locs := m.Definition(context.Background(), file, 1, 1); locs != nil {
		t.Fatalf("expected no answer from a broken server, got %#v", locs)
	}
	m.mu.Lock()
	failed := len(m.failed)
	m.mu.Unlock()
	if failed != 1 {
		t.Fatalf("expected the failure to be remembered, got %d entries", failed)
	}
}

func TestReadHeaderRejectsMissingLength(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("Content-Type: application/json\r\n\r\n"))
	if _, err := readHeader(br); err == nil {
		t.Fatal("expected an error when Content-Length is absent")
	}
}

func TestDecodeLocationsAcceptsEveryShape(t *testing.T) {
	single := `{"uri":"file:///a.go","range":{"start":{"line":1,"character":2}}}`
	array := `[` + single + `]`
	links := `[{"targetUri":"file:///a.go","targetSelectionRange":{"start":{"line":1,"character":2}}}]`
	for name, raw := range map[string]string{"single": single, "array": array, "links": links} {
		locs := decodeLocations(json.RawMessage(raw))
		if len(locs) != 1 || locs[0].URI != "file:///a.go" || locs[0].Range.Start.Line != 1 {
			t.Fatalf("%s: unexpected decode %#v", name, locs)
		}
	}
	if locs := decodeLocations(json.RawMessage("null")); locs != nil {
		t.Fatalf("null should decode to nothing, got %#v", locs)
	}
}

func TestDecodeHoverFlattensShapes(t *testing.T) {
	cases := map[string]string{
		`{"contents":"plain"}`:                              "plain",
		`{"contents":{"kind":"markdown","value":"marked"}}`: "marked",
		`{"contents":["one","two"]}`:                        "one\ntwo",
	}
	for raw, want := range cases {
		if got := decodeHover(json.RawMessage(raw)); got != want {
			t.Fatalf("%s: got %q want %q", raw, got, want)
		}
	}
}

func TestURIRoundTripEscapesSpaces(t *testing.T) {
	path := "/tmp/my project/main.go"
	uri := pathToURI(path)
	if strings.Contains(uri, " ") {
		t.Fatalf("space left unescaped in %q", uri)
	}
	if got := uriToPath(uri); got != path {
		t.Fatalf("round trip lost the path: %q", got)
	}
}

func TestRootForWalksUpToMarker(t *testing.T) {
	workspace := t.TempDir()
	nested := filepath.Join(workspace, "svc", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "svc", "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := Server{RootMarkers: []string{"go.mod"}}
	got := server.rootFor(filepath.Join(nested, "main.go"), workspace)
	if got != filepath.Join(workspace, "svc") {
		t.Fatalf("expected the nested module root, got %q", got)
	}
	// No marker anywhere: fall back to the workspace rather than escaping it.
	bare := Server{RootMarkers: []string{"Cargo.toml"}}
	if got := bare.rootFor(filepath.Join(nested, "main.go"), workspace); got != workspace {
		t.Fatalf("expected the workspace root, got %q", got)
	}
}

func TestMergeServersOverridesByName(t *testing.T) {
	merged := mergeServers(Builtins(), map[string]Server{
		"gopls":  {Command: "/opt/gopls", Extensions: []string{".go"}},
		"custom": {Command: "zls", Extensions: []string{".zig"}},
	})
	var gopls, custom *Server
	for i := range merged {
		switch merged[i].Name {
		case "gopls":
			gopls = &merged[i]
		case "custom":
			custom = &merged[i]
		}
	}
	if gopls == nil || gopls.Command != "/opt/gopls" {
		t.Fatalf("built-in was not overridden: %#v", gopls)
	}
	if custom == nil || custom.Command != "zls" {
		t.Fatalf("extra server was not added: %#v", custom)
	}
	if len(merged) != len(Builtins())+1 {
		t.Fatalf("override should replace, not append: %d servers", len(merged))
	}
}

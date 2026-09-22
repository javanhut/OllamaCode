package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestExternalMCPServerLifecycleAndToolCall(t *testing.T) {
	server, err := NewExternalServer("fixture", os.Args[0], "-test.run=TestMCPHelperProcess", "--", "--mcp-helper")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
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
	got, err := definitions[0].Handler(ctx, json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "echo: hello" {
		t.Fatalf("unexpected MCP result: %q", got)
	}
}

func TestExternalToolNameIsProviderSafe(t *testing.T) {
	name := externalToolName("server with spaces", "a/very/long/tool/name/that/keeps/going/until/provider/limits/would/be/exceeded")
	if len(name) > 64 || nonToolName.MatchString(name) {
		t.Fatalf("unsafe external tool name %q", name)
	}
}

func TestExternalToolLongServerKeepsNamespace(t *testing.T) {
	server := strings.Repeat("long server ", 20)
	name := externalToolName(server, strings.Repeat("remote", 20))
	if len(name) > 64 || !strings.HasPrefix(name, externalNamespace(server)) {
		t.Fatalf("tool %q escaped namespace %q", name, externalNamespace(server))
	}
}

func TestExternalServerReceivesToolListChanged(t *testing.T) {
	server, err := NewExternalServer("fixture", os.Args[0], "-test.run=TestMCPHelperProcess", "--", "--mcp-helper")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	changed := make(chan struct{}, 1)
	server.SetToolsChangedHandler(func() { changed <- struct{}{} })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Initialize(ctx, "2025-11-25"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("tools/list_changed notification was not delivered")
	}
}

func TestAllowedEnvironmentIsExplicit(t *testing.T) {
	t.Setenv("MCP_ALLOWED_TEST", "yes")
	t.Setenv("MCP_SECRET_TEST", "no")
	env := allowedEnvironment([]string{"MCP_ALLOWED_TEST"})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "MCP_ALLOWED_TEST=yes") || strings.Contains(joined, "MCP_SECRET_TEST") {
		t.Fatalf("unexpected child environment: %v", env)
	}
}

func TestMCPHelperProcess(t *testing.T) {
	if !slices.Contains(os.Args, "--mcp-helper") {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			continue
		}
		if len(req.ID) == 0 {
			if req.Method == "notifications/initialized" {
				fmt.Println(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
			}
			continue
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
					"required":   []string{"text"},
				},
			}}}
		case "tools/call":
			var params struct {
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &params)
			result = map[string]any{"content": []any{map[string]any{
				"type": "text", "text": "echo: " + fmt.Sprint(params.Arguments["text"]),
			}}}
		default:
			continue
		}
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": result})
		fmt.Println(string(response))
	}
}

package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

const defaultMCPProtocolVersion = "2025-11-25"

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      int             `json:"id,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
	ID      json.RawMessage `json:"id"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type externalResult struct {
	response *JSONRPCResponse
	err      error
}

// ExternalServer owns one stateful MCP stdio subprocess. Requests are
// serialized onto stdin while responses may arrive out of order and are routed
// by their JSON-RPC id.
type ExternalServer struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[string]chan externalResult
	nextID    int
	done      chan struct{}
	errTail   strings.Builder
	closeOnce sync.Once
}

func NewExternalServer(name, command string, args ...string) (*ExternalServer, error) {
	if strings.TrimSpace(command) == "" {
		return nil, fmt.Errorf("MCP server %q has no command", name)
	}
	cmd := exec.Command(command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	s := &ExternalServer{
		name: name, cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr,
		pending: map[string]chan externalResult{}, done: make(chan struct{}),
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go s.listen()
	go s.captureStderr()
	return s, nil
}

func (s *ExternalServer) listen() {
	scanner := bufio.NewScanner(s.stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var resp JSONRPCResponse
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil || len(resp.ID) == 0 {
			continue // notification or malformed server output
		}
		key := string(bytes.TrimSpace(resp.ID))
		s.mu.Lock()
		ch := s.pending[key]
		delete(s.pending, key)
		s.mu.Unlock()
		if ch != nil {
			ch <- externalResult{response: &resp}
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	s.failPending(fmt.Errorf("MCP server %q stopped: %w%s", s.name, err, s.stderrSuffix()))
	close(s.done)
}

func (s *ExternalServer) captureStderr() {
	scanner := bufio.NewScanner(s.stderr)
	for scanner.Scan() {
		s.mu.Lock()
		if s.errTail.Len() > 16*1024 {
			existing := s.errTail.String()
			s.errTail.Reset()
			s.errTail.WriteString(existing[len(existing)-8*1024:])
		}
		s.errTail.WriteString("\n" + scanner.Text())
		s.mu.Unlock()
	}
}

func (s *ExternalServer) stderrSuffix() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.errTail.Len() == 0 {
		return ""
	}
	return ":" + s.errTail.String()
}

func (s *ExternalServer) failPending(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, ch := range s.pending {
		delete(s.pending, key)
		ch <- externalResult{err: err}
	}
}

func (s *ExternalServer) send(value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = fmt.Fprintln(s.stdin, string(b))
	return err
}

func (s *ExternalServer) notify(method string, params any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return s.send(JSONRPCRequest{JSONRPC: "2.0", Method: method, Params: p})
}

func (s *ExternalServer) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	key := fmt.Sprint(id)
	ch := make(chan externalResult, 1)
	s.pending[key] = ch
	s.mu.Unlock()

	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if err := s.send(JSONRPCRequest{JSONRPC: "2.0", Method: method, Params: p, ID: id}); err != nil {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		_ = s.notify("notifications/cancelled", map[string]any{"requestId": id, "reason": ctx.Err().Error()})
		return nil, ctx.Err()
	case result := <-ch:
		if result.err != nil {
			return nil, result.err
		}
		if result.response.Error != nil {
			return nil, fmt.Errorf("MCP rpc error (%d): %s", result.response.Error.Code, result.response.Error.Message)
		}
		return result.response.Result, nil
	}
}

// Initialize performs the required stateful MCP capability handshake.
func (s *ExternalServer) Initialize(ctx context.Context, protocolVersion string) error {
	if protocolVersion == "" {
		protocolVersion = defaultMCPProtocolVersion
	}
	result, err := s.Call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "OllamaCode", "version": "1"},
	})
	if err != nil {
		return err
	}
	var initialized struct {
		ProtocolVersion string                     `json:"protocolVersion"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(result, &initialized); err != nil {
		return fmt.Errorf("invalid MCP initialize response: %w", err)
	}
	if initialized.ProtocolVersion == "" {
		return fmt.Errorf("MCP server %q returned no protocol version", s.name)
	}
	if _, ok := initialized.Capabilities["tools"]; !ok {
		return fmt.Errorf("MCP server %q does not advertise tools", s.name)
	}
	return s.notify("notifications/initialized", map[string]any{})
}

func (s *ExternalServer) Close() error {
	var err error
	s.closeOnce.Do(func() {
		_ = s.stdin.Close()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			_ = s.cmd.Process.Kill()
			<-s.done
		}
		err = s.cmd.Wait()
	})
	return err
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

var nonToolName = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func externalToolName(server, remote string) string {
	name := "mcp_" + nonToolName.ReplaceAllString(server, "_") + "_" + nonToolName.ReplaceAllString(remote, "_")
	name = strings.Trim(name, "_")
	if len(name) <= 64 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:55] + fmt.Sprintf("_%x", sum[:4])
}

// ListTools retrieves every page of tools and adapts their MCP input schemas
// to OllamaCode's internal function definitions.
func (s *ExternalServer) ListTools(ctx context.Context, policy ToolPolicy) ([]Tool, error) {
	var out []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		resp, err := s.Call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var result struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(resp, &result); err != nil {
			return nil, err
		}
		for _, remote := range result.Tools {
			remote := remote
			fn, err := functionFromMCPSchema(externalToolName(s.name, remote.Name), remote.Description, remote.InputSchema)
			if err != nil {
				return nil, fmt.Errorf("MCP tool %s/%s: %w", s.name, remote.Name, err)
			}
			out = append(out, Tool{Type: "function", Function: fn, Policy: policy,
				Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
					return s.callTool(ctx, remote.Name, args)
				}})
		}
		if result.NextCursor == "" {
			return out, nil
		}
		cursor = result.NextCursor
	}
}

func (s *ExternalServer) callTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	var arguments map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return "", err
		}
	}
	resp, err := s.Call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return "", err
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", err
	}
	var parts []string
	for _, content := range result.Content {
		if content.Type == "text" && content.Text != "" {
			parts = append(parts, content.Text)
		}
	}
	if len(result.StructuredContent) > 0 {
		parts = append(parts, string(result.StructuredContent))
	}
	text := strings.Join(parts, "\n")
	if result.IsError {
		return "", fmt.Errorf("%s", text)
	}
	return text, nil
}

func functionFromMCPSchema(name, description string, raw json.RawMessage) (Function, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	var schema Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return Function{}, fmt.Errorf("invalid inputSchema: %w", err)
	}
	if schema.Type == "" {
		schema.Type = "object"
	}
	if schema.Properties == nil {
		schema.Properties = map[string]Property{}
	}
	return Function{Name: name, Description: description, Parameters: schema}, nil
}

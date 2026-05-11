package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// JSONRPCRequest represents a standard JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      any             `json:"id,omitempty"`
}

// JSONRPCResponse represents a standard JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
	ID      any             `json:"id"`
}

// JSONRPCError represents a standard JSON-RPC 2.0 error.
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ExternalServer manages a connection to an external MCP server via stdio.
type ExternalServer struct {
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	mu      sync.Mutex
	pending map[any]chan *JSONRPCResponse
	nextID  int
}

func NewExternalServer(name string, command string, args ...string) (*ExternalServer, error) {
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
		name:    name,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		pending: make(map[any]chan *JSONRPCResponse),
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go s.listen()
	return s, nil
}

func (s *ExternalServer) listen() {
	scanner := bufio.NewScanner(s.stdout)
	for scanner.Scan() {
		var resp JSONRPCResponse
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			continue
		}

		s.mu.Lock()
		ch, ok := s.pending[resp.ID]
		if ok {
			delete(s.pending, resp.ID)
			ch <- &resp
		}
		s.mu.Unlock()
	}
}

func (s *ExternalServer) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	ch := make(chan *JSONRPCResponse, 1)
	s.pending[id] = ch
	s.mu.Unlock()

	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	req := JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  p,
		ID:      id,
	}

	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	if _, err := fmt.Fprintln(s.stdin, string(b)); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("rpc error (%d): %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (s *ExternalServer) Close() error {
	s.stdin.Close()
	return s.cmd.Wait()
}

// ListTools retrieves the list of tools provided by the external server.
func (s *ExternalServer) ListTools(ctx context.Context) ([]Tool, error) {
	resp, err := s.Call(ctx, "listTools", nil)
	if err != nil {
		return nil, err
	}

	var result struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, err
	}

	// Wrap each tool with a handler that calls back to the external server
	for i := range result.Tools {
		name := result.Tools[i].Function.Name
		result.Tools[i].Handler = func(ctx context.Context, args json.RawMessage) (string, error) {
			callResp, err := s.Call(ctx, "callTool", map[string]any{
				"name":      name,
				"arguments": args,
			})
			if err != nil {
				return "", err
			}

			var callResult struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			}
			if err := json.Unmarshal(callResp, &callResult); err != nil {
				return "", err
			}

			var out []string
			for _, c := range callResult.Content {
				if c.Type == "text" {
					out = append(out, c.Text)
				}
			}
			full := strings.Join(out, "\n")
			if callResult.IsError {
				return "", fmt.Errorf("%s", full)
			}
			return full, nil
		}
	}

	return result.Tools, nil
}

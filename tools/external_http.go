package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type HTTPExternalServerOptions struct {
	Name             string
	URL              string
	Headers          map[string]string
	MaxResponseBytes int
	CallTimeout      time.Duration
	Client           *http.Client
}

// HTTPExternalServer implements MCP Streamable HTTP. It accepts JSON and SSE
// responses, carries the negotiated session id, listens for server-initiated
// notifications over GET, and closes the remote session with DELETE.
type HTTPExternalServer struct {
	name             string
	url              string
	headers          map[string]string
	client           *http.Client
	maxResponseBytes int
	callTimeout      time.Duration

	mu             sync.Mutex
	nextID         int
	sessionID      string
	onToolsChanged func()
	cancelEvents   context.CancelFunc
	done           chan struct{}
	closeOnce      sync.Once
	changeMu       sync.Mutex
}

func NewHTTPExternalServer(opts HTTPExternalServerOptions) (*HTTPExternalServer, error) {
	if strings.TrimSpace(opts.Name) == "" || strings.TrimSpace(opts.URL) == "" {
		return nil, fmt.Errorf("MCP HTTP server requires name and URL")
	}
	if opts.MaxResponseBytes <= 0 {
		opts.MaxResponseBytes = 4 * 1024 * 1024
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = 2 * time.Minute
	}
	if opts.Client == nil {
		opts.Client = &http.Client{}
	}
	return &HTTPExternalServer{name: opts.Name, url: opts.URL, headers: cloneStrings(opts.Headers), client: opts.Client,
		maxResponseBytes: opts.MaxResponseBytes, callTimeout: opts.CallTimeout, done: make(chan struct{})}, nil
}

func cloneStrings(input map[string]string) map[string]string {
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func (s *HTTPExternalServer) Namespace() string     { return externalNamespace(s.name) }
func (s *HTTPExternalServer) Done() <-chan struct{} { return s.done }
func (s *HTTPExternalServer) SetToolsChangedHandler(handler func()) {
	s.mu.Lock()
	s.onToolsChanged = handler
	s.mu.Unlock()
}

func (s *HTTPExternalServer) Initialize(ctx context.Context, protocolVersion string) error {
	if protocolVersion == "" {
		protocolVersion = defaultMCPProtocolVersion
	}
	result, err := s.Call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion, "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "OllamaCode", "version": "1"},
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
	if err := s.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return err
	}
	s.startEventStream()
	return nil
}

func (s *HTTPExternalServer) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, s.callTimeout)
	defer cancel()
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()
	request := JSONRPCRequest{JSONRPC: "2.0", Method: method, ID: id}
	request.Params, _ = json.Marshal(params)
	response, err := s.send(ctx, http.MethodPost, request)
	if err != nil {
		return nil, err
	}
	var rpc JSONRPCResponse
	if err := json.Unmarshal(response, &rpc); err != nil {
		return nil, fmt.Errorf("invalid MCP JSON-RPC response: %w", err)
	}
	if rpc.Error != nil {
		return nil, fmt.Errorf("MCP rpc error (%d): %s", rpc.Error.Code, rpc.Error.Message)
	}
	return rpc.Result, nil
}

func (s *HTTPExternalServer) notify(ctx context.Context, method string, params any) error {
	request := JSONRPCRequest{JSONRPC: "2.0", Method: method}
	request.Params, _ = json.Marshal(params)
	_, err := s.send(ctx, http.MethodPost, request)
	return err
}

func (s *HTTPExternalServer) send(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	s.mu.Lock()
	sessionID := s.sessionID
	s.mu.Unlock()
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	for key, value := range s.headers {
		req.Header.Set(key, value)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		s.mu.Lock()
		s.sessionID = id
		s.mu.Unlock()
	}
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("MCP HTTP status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	data, err := readBounded(resp.Body, s.maxResponseBytes)
	if err != nil {
		return nil, err
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return lastSSEData(data)
	}
	return data, nil
}

func readBounded(reader io.Reader, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("MCP response exceeded %d bytes", limit)
	}
	return data, nil
}

func lastSSEData(data []byte) (json.RawMessage, error) {
	var last string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "data:") {
			last = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if last == "" {
		return nil, fmt.Errorf("MCP SSE response contained no data event")
	}
	return json.RawMessage(last), nil
}

func (s *HTTPExternalServer) startEventStream() {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancelEvents = cancel
	s.mu.Unlock()
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		s.mu.Lock()
		id := s.sessionID
		s.mu.Unlock()
		if id != "" {
			req.Header.Set("Mcp-Session-Id", id)
		}
		for key, value := range s.headers {
			req.Header.Set(key, value)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return
		}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), s.maxResponseBytes)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var notification struct {
				Method string `json:"method"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &notification) == nil && notification.Method == "notifications/tools/list_changed" {
				s.dispatchToolsChanged()
			}
		}
	}()
}

func (s *HTTPExternalServer) dispatchToolsChanged() {
	s.mu.Lock()
	handler := s.onToolsChanged
	s.mu.Unlock()
	if handler == nil {
		return
	}
	go func() { s.changeMu.Lock(); defer s.changeMu.Unlock(); handler() }()
}

func (s *HTTPExternalServer) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		cancel := s.cancelEvents
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_, closeErr = s.send(ctx, http.MethodDelete, nil)
		close(s.done)
	})
	return closeErr
}

func (s *HTTPExternalServer) ListTools(ctx context.Context, policy ToolPolicy) ([]Tool, error) {
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
			out = append(out, Tool{Type: "function", Function: fn, Policy: policy, Handler: func(callCtx context.Context, args json.RawMessage) (string, error) {
				return s.callTool(callCtx, remote.Name, args)
			}})
		}
		if result.NextCursor == "" {
			return out, nil
		}
		cursor = result.NextCursor
	}
}

func (s *HTTPExternalServer) callTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
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
	return decodeMCPToolResult(resp)
}

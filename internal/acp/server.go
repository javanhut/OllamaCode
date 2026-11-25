// Package acp implements the agent side of the Agent Client Protocol
// (https://agentclientprotocol.com): an editor launches `ocode acp` and drives
// it over JSON-RPC 2.0 on stdio. Implemented against protocol version 1.
//
// The package knows nothing about config or models; the caller supplies a
// RunFunc that runs one agent turn (tui.NewACPRunner in production, a fake
// chat client in tests).
package acp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/tools"
)

// ProtocolVersion is the ACP major version this adapter speaks.
const ProtocolVersion = 1

// RunFunc runs one agent turn in cwd. history is the session's prior
// conversation (nil on the first prompt); mod must be applied to the
// agent.Options before agent.Run so the adapter's hooks (streaming, tool
// reporting, Approve) are installed. The returned Result.Messages becomes the
// next turn's history.
type RunFunc func(ctx context.Context, cwd, prompt string, history []api.Message, mod func(*agent.Options)) (agent.Result, error)

// Server is one ACP agent connection.
type Server struct {
	Run     RunFunc
	Name    string // agentInfo.name; "ocode" when empty
	Version string // agentInfo.version
}

type session struct {
	id  string
	cwd string

	mu      sync.Mutex
	history []api.Message
	cancel  context.CancelFunc // non-nil while a turn runs
	rules   []tools.PermissionRule
}

// permReply is the client's answer to session/request_permission:
// {"outcome": {"outcome": "selected", "optionId": "..."}} or
// {"outcome": {"outcome": "cancelled"}}.
type permReply struct {
	outcome struct {
		Outcome  string `json:"outcome"`
		OptionID string `json:"optionId"`
	}
	err error
}

type conn struct {
	srv *Server
	w   *writer
	ctx context.Context

	mu       sync.Mutex
	sessions map[string]*session
	pending  map[string]chan permReply
	nextID   atomic.Int64

	// turnSem serializes turns across sessions: workspace root, cwd and the
	// tool jail are process-global.
	turnSem chan struct{}
	wg      sync.WaitGroup
}

// Serve reads requests from r and writes responses to w until r closes or ctx
// ends. Running turns are cancelled and awaited before it returns.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := &conn{srv: s, w: &writer{w: w}, ctx: ctx,
		sessions: map[string]*session{}, pending: map[string]chan permReply{},
		turnSem: make(chan struct{}, 1)}
	defer c.wg.Wait()
	defer cancel()

	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := readLine(br)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			_ = c.w.respondError(nil, codeParseError, "parse error: "+err.Error())
			continue
		}
		switch {
		case m.Method == "" && m.hasID() && (m.Result != nil || m.Error != nil):
			c.deliver(m)
		case m.Method == "":
			_ = c.w.respondError(m.ID, codeInvalidRequest, "invalid request: missing method")
		default:
			c.dispatch(m)
		}
	}
}

func (c *conn) deliver(m message) {
	c.mu.Lock()
	ch := c.pending[string(m.ID)]
	delete(c.pending, string(m.ID))
	c.mu.Unlock()
	if ch == nil {
		return
	}
	var reply permReply
	if m.Error != nil {
		reply.err = m.Error
	} else {
		var r struct {
			Outcome json.RawMessage `json:"outcome"`
		}
		if err := json.Unmarshal(m.Result, &r); err == nil {
			err = json.Unmarshal(r.Outcome, &reply.outcome)
		}
		if reply.outcome.Outcome == "" {
			reply.err = fmt.Errorf("bad permission response: %s", m.Result)
		}
	}
	ch <- reply // buffered
}

func (c *conn) fail(m message, code int, msg string) {
	if m.hasID() {
		_ = c.w.respondError(m.ID, code, msg)
	}
}

func (c *conn) dispatch(m message) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion int `json:"protocolVersion"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil || p.ProtocolVersion < 1 {
			c.fail(m, codeInvalidParams, "initialize requires protocolVersion >= 1")
			return
		}
		name := c.srv.Name
		if name == "" {
			name = "ocode"
		}
		_ = c.w.respond(m.ID, map[string]any{
			"protocolVersion": ProtocolVersion,
			"agentCapabilities": map[string]any{
				"loadSession": false,
				"promptCapabilities": map[string]any{
					"image": false, "audio": false, "embeddedContext": true,
				},
				"mcpCapabilities": map[string]any{"http": false, "sse": false},
			},
			"agentInfo":   map[string]any{"name": name, "title": "ocode", "version": c.srv.Version},
			"authMethods": []any{},
		})
	case "session/new":
		var p struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil || !filepath.IsAbs(p.Cwd) {
			c.fail(m, codeInvalidParams, "session/new requires an absolute cwd")
			return
		}
		if st, err := os.Stat(p.Cwd); err != nil || !st.IsDir() {
			c.fail(m, codeInvalidParams, "cwd is not a directory: "+p.Cwd)
			return
		}
		var b [8]byte
		_, _ = rand.Read(b[:])
		s := &session{id: "sess_" + hex.EncodeToString(b[:]), cwd: p.Cwd}
		c.mu.Lock()
		c.sessions[s.id] = s
		c.mu.Unlock()
		_ = c.w.respond(m.ID, map[string]any{"sessionId": s.id})
	case "session/prompt":
		c.prompt(m)
	case "session/cancel":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			if s := c.session(p.SessionID); s != nil {
				s.mu.Lock()
				if s.cancel != nil {
					s.cancel()
				}
				s.mu.Unlock()
			}
		}
	default:
		c.fail(m, codeMethodNotFound, "method not found: "+m.Method)
	}
}

func (c *conn) session(id string) *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[id]
}

type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	URI      string `json:"uri"`
	Name     string `json:"name"`
	Resource *struct {
		URI  string `json:"uri"`
		Text string `json:"text"`
	} `json:"resource"`
}

// promptText flattens ACP content blocks into the model's user message.
func promptText(blocks []contentBlock) (string, error) {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "resource_link":
			parts = append(parts, "[referenced file: "+b.URI+"]")
		case "resource":
			if b.Resource == nil || b.Resource.Text == "" {
				return "", fmt.Errorf("embedded resource without text is not supported")
			}
			parts = append(parts, "<context uri=\""+b.Resource.URI+"\">\n"+b.Resource.Text+"\n</context>")
		default:
			return "", fmt.Errorf("content block type %q is not supported", b.Type)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

func (c *conn) prompt(m message) {
	var p struct {
		SessionID string         `json:"sessionId"`
		Prompt    []contentBlock `json:"prompt"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		c.fail(m, codeInvalidParams, "invalid session/prompt params: "+err.Error())
		return
	}
	s := c.session(p.SessionID)
	if s == nil {
		c.fail(m, codeInvalidParams, "unknown session: "+p.SessionID)
		return
	}
	text, err := promptText(p.Prompt)
	if err != nil {
		c.fail(m, codeInvalidParams, err.Error())
		return
	}
	if strings.TrimSpace(text) == "" {
		c.fail(m, codeInvalidParams, "empty prompt")
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		cancel()
		c.fail(m, codeInvalidRequest, "a prompt turn is already running for this session")
		return
	}
	s.cancel = cancel
	s.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		stop, err := c.turn(ctx, s, text)
		// Release the session BEFORE responding: a client may send its next
		// prompt the moment it sees this response, and must not be told a
		// turn is still running.
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
		cancel()
		if err != nil {
			c.fail(m, codeInternal, err.Error())
			return
		}
		_ = c.w.respond(m.ID, map[string]any{"stopReason": stop})
	}()
}

// turn runs one prompt through the agent loop and maps its outcome to a
// stopReason.
func (c *conn) turn(ctx context.Context, s *session, text string) (string, error) {
	select {
	case c.turnSem <- struct{}{}:
		defer func() { <-c.turnSem }()
	case <-ctx.Done():
		return "cancelled", nil
	}
	tools.SetWorkspaceRoot(s.cwd)
	if err := os.Chdir(s.cwd); err != nil {
		return "", fmt.Errorf("enter workspace %s: %w", s.cwd, err)
	}
	t := &turnState{c: c, s: s}
	s.mu.Lock()
	history := s.history
	s.mu.Unlock()
	res, err := c.srv.Run(ctx, s.cwd, text, history, t.install)
	if len(res.Messages) > 0 {
		s.mu.Lock()
		s.history = res.Messages
		s.mu.Unlock()
	}
	switch {
	case ctx.Err() != nil:
		return "cancelled", nil
	case err != nil:
		return "", err
	case res.HitLimit:
		return "max_turn_requests", nil
	}
	return "end_turn", nil
}

func (c *conn) update(sessionID string, update map[string]any) {
	_ = c.w.notify("session/update", map[string]any{"sessionId": sessionID, "update": update})
}

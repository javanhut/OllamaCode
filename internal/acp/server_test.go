package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/javanhut/ollama_code/api"
	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/tools"
)

// scriptedChat returns canned responses in order; once exhausted it blocks
// until the context ends (a model that never answers, for cancel tests).
type scriptedChat struct {
	mu        sync.Mutex
	responses []api.ChatResponse
	calls     int
	block     bool
}

func (f *scriptedChat) ChatOnce(ctx context.Context, _ api.ChatRequest) (api.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.block || f.calls >= len(f.responses) {
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return api.ChatResponse{}, ctx.Err()
	}
	r := f.responses[f.calls]
	f.calls++
	return r, nil
}

func text(s string) api.ChatResponse { return api.ChatResponse{Message: api.Message{Content: s}} }

func toolCall(name, args string) api.ChatResponse {
	return api.ChatResponse{Message: api.Message{ToolCalls: []tools.ToolCall{
		{Function: tools.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}}}}
}

type harness struct {
	t       *testing.T
	in      io.WriteCloser
	out     chan message
	dir     string
	done    chan error
	stopped chan struct{} // closed when Serve returns (done is consumed by some tests)
	nextID  int
}

func newHarness(t *testing.T, chat agent.ChatClient) *harness {
	t.Helper()
	wd, _ := os.Getwd()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd); tools.SetWorkspaceRoot("") })

	reg := tools.NewRegistry()
	reg.Register(tools.WriteFileTool())
	run := func(ctx context.Context, cwd, prompt string, history []api.Message, mod func(*agent.Options)) (agent.Result, error) {
		opts := agent.Options{Model: "m", System: "sys", PriorMessages: history}
		mod(&opts)
		return agent.Run(ctx, chat, reg, prompt, opts)
	}
	srvIn, clientIn := io.Pipe()
	clientOut, srvOut := io.Pipe()
	h := &harness{t: t, in: clientIn, out: make(chan message, 64), dir: dir, done: make(chan error, 1), stopped: make(chan struct{})}
	go func() {
		err := (&Server{Run: run, Version: "test"}).Serve(context.Background(), srvIn, srvOut)
		_ = srvOut.Close()
		h.done <- err
		close(h.stopped)
	}()
	go func() {
		sc := bufio.NewScanner(clientOut)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m message
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("server wrote invalid JSON %q: %v", sc.Text(), err)
				continue
			}
			h.out <- m
		}
		close(h.out)
	}()
	// Close the input and WAIT for Serve to return: its turns chdir into the
	// session's workspace, and a server outliving its test can move the cwd
	// under the next test (a relative write then lands in the wrong temp dir).
	// Registered after the chdir-restoring cleanup, so it runs first (LIFO).
	t.Cleanup(func() {
		_ = h.in.Close()
		select {
		case <-h.stopped:
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down after its input closed")
		}
	})
	return h
}

func (h *harness) sendRaw(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.in, line+"\n"); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) send(v any) {
	h.t.Helper()
	data, _ := json.Marshal(v)
	h.sendRaw(string(data))
}

func (h *harness) notify(method string, params any) {
	h.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (h *harness) next() message {
	h.t.Helper()
	select {
	case m, ok := <-h.out:
		if !ok {
			h.t.Fatal("server closed output")
		}
		return m
	case <-time.After(10 * time.Second):
		h.t.Fatal("timed out waiting for server message")
	}
	return message{}
}

// request sends a request and returns its response plus every notification
// that arrived first. Server-initiated requests go to onReq, whose return
// value becomes the response result.
func (h *harness) request(method string, params any, onReq func(message) any) (message, []message) {
	h.t.Helper()
	h.nextID++
	id := h.nextID
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return h.await(id, onReq)
}

func (h *harness) await(id int, onReq func(message) any) (message, []message) {
	h.t.Helper()
	var updates []message
	for {
		m := h.next()
		switch {
		case m.Method != "" && m.hasID():
			if onReq == nil {
				h.t.Fatalf("unexpected server request %s", m.Method)
			}
			h.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(m.ID), "result": onReq(m)})
		case m.Method != "":
			updates = append(updates, m)
		case string(m.ID) == itoa(id):
			return m, updates
		}
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func (h *harness) newSession() string {
	h.t.Helper()
	resp, _ := h.request("session/new", map[string]any{"cwd": h.dir, "mcpServers": []any{}}, nil)
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if resp.Error != nil || json.Unmarshal(resp.Result, &r) != nil || r.SessionID == "" {
		h.t.Fatalf("session/new failed: %+v", resp)
	}
	return r.SessionID
}

func promptParams(sid, msg string) map[string]any {
	return map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": msg}}}
}

func stopReason(t *testing.T, m message) string {
	t.Helper()
	var r struct {
		StopReason string `json:"stopReason"`
	}
	if m.Error != nil || json.Unmarshal(m.Result, &r) != nil {
		t.Fatalf("prompt failed: %+v", m)
	}
	return r.StopReason
}

type update struct {
	Update struct {
		Kind       string `json:"sessionUpdate"`
		ToolCallID string `json:"toolCallId"`
		Status     string `json:"status"`
		Kindv      string `json:"kind"`
		Content    json.RawMessage
	} `json:"update"`
}

func parseUpdates(t *testing.T, ms []message) []update {
	var out []update
	for _, m := range ms {
		if m.Method != "session/update" {
			t.Fatalf("unexpected notification %s", m.Method)
		}
		var u update
		if err := json.Unmarshal(m.Params, &u); err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	return out
}

func selectOption(id string) func(message) any {
	return func(m message) any {
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": id}}
	}
}

func TestInitialize(t *testing.T) {
	h := newHarness(t, &scriptedChat{})
	resp, _ := h.request("initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}, nil)
	var r struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession        bool            `json:"loadSession"`
			PromptCapabilities map[string]bool `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
		AgentInfo struct{ Name string } `json:"agentInfo"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil || resp.Error != nil {
		t.Fatalf("bad response %+v", resp)
	}
	if r.ProtocolVersion != 1 || r.AgentCapabilities.LoadSession || r.AgentCapabilities.PromptCapabilities["image"] || r.AgentCapabilities.PromptCapabilities["audio"] || r.AgentInfo.Name != "ocode" {
		t.Fatalf("capabilities overstated: %s", resp.Result)
	}
}

func TestSessionNewRejectsBadCwd(t *testing.T) {
	h := newHarness(t, &scriptedChat{})
	for _, cwd := range []string{"relative/dir", filepath.Join(h.dir, "missing")} {
		resp, _ := h.request("session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}}, nil)
		if resp.Error == nil || resp.Error.Code != codeInvalidParams {
			t.Fatalf("cwd %q: want invalid params, got %+v", cwd, resp)
		}
	}
}

func TestPromptStreamsText(t *testing.T) {
	h := newHarness(t, &scriptedChat{responses: []api.ChatResponse{text("hello from ocode")}})
	sid := h.newSession()
	resp, ups := h.request("session/prompt", promptParams(sid, "hi"), nil)
	if got := stopReason(t, resp); got != "end_turn" {
		t.Fatalf("stopReason = %s", got)
	}
	us := parseUpdates(t, ups)
	if len(us) != 1 || us[0].Update.Kind != "agent_message_chunk" || !strings.Contains(string(ups[0].Params), "hello from ocode") {
		t.Fatalf("updates = %s", ups[0].Params)
	}
}

func TestPermissionApproveWritesFile(t *testing.T) {
	chat := &scriptedChat{responses: []api.ChatResponse{toolCall("write_file", `{"path":"out.txt","content":"data"}`), text("done")}}
	h := newHarness(t, chat)
	sid := h.newSession()
	var asked []message
	resp, ups := h.request("session/prompt", promptParams(sid, "write"), func(m message) any {
		asked = append(asked, m)
		if m.Method != "session/request_permission" {
			t.Fatalf("method %s", m.Method)
		}
		return selectOption("allow_once")(m)
	})
	if stopReason(t, resp) != "end_turn" || len(asked) != 1 {
		t.Fatalf("resp=%+v asked=%d", resp, len(asked))
	}
	data, err := os.ReadFile(filepath.Join(h.dir, "out.txt"))
	if err != nil || string(data) != "data" {
		t.Fatalf("file not written: %v %q", err, data)
	}
	var req struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
		} `json:"toolCall"`
		Options []struct{ Kind string } `json:"options"`
	}
	_ = json.Unmarshal(asked[0].Params, &req)
	kinds := map[string]bool{}
	for _, o := range req.Options {
		kinds[o.Kind] = true
	}
	if req.ToolCall.ToolCallID != "call_1" || !kinds["allow_once"] || !kinds["allow_always"] || !kinds["reject_once"] || !kinds["reject_always"] {
		t.Fatalf("bad permission request: %s", asked[0].Params)
	}
	us := parseUpdates(t, ups)
	var statuses []string
	for _, u := range us {
		if u.Update.ToolCallID == "call_1" {
			statuses = append(statuses, u.Update.Kind+":"+u.Update.Status)
		}
	}
	if strings.Join(statuses, ",") != "tool_call:pending,tool_call_update:in_progress,tool_call_update:completed" {
		t.Fatalf("tool lifecycle = %v", statuses)
	}
}

func TestPermissionRejectSkipsWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(message) any
	}{
		{"reject_once", selectOption("reject_once")},
		{"cancelled outcome", func(message) any { return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := &scriptedChat{responses: []api.ChatResponse{toolCall("write_file", `{"path":"nope.txt","content":"x"}`), text("ok, skipped")}}
			h := newHarness(t, chat)
			sid := h.newSession()
			resp, ups := h.request("session/prompt", promptParams(sid, "write"), tc.answer)
			if stopReason(t, resp) != "end_turn" {
				t.Fatalf("resp %+v", resp)
			}
			if _, err := os.Stat(filepath.Join(h.dir, "nope.txt")); err == nil {
				t.Fatal("rejected write happened")
			}
			failed := false
			for _, u := range parseUpdates(t, ups) {
				failed = failed || u.Update.Status == "failed"
			}
			if !failed {
				t.Fatal("rejected call not reported as failed")
			}
		})
	}
}

func TestAllowAlwaysSkipsSecondPrompt(t *testing.T) {
	chat := &scriptedChat{responses: []api.ChatResponse{
		toolCall("write_file", `{"path":"a.txt","content":"1"}`), text("one"),
		toolCall("write_file", `{"path":"b.txt","content":"2"}`), text("two"),
	}}
	h := newHarness(t, chat)
	sid := h.newSession()
	asked := 0
	answer := func(m message) any { asked++; return selectOption("allow_always")(m) }
	h.request("session/prompt", promptParams(sid, "a"), answer)
	// Sent the moment the first response arrives: the session must already
	// be released (a client is allowed to do exactly this).
	if resp, _ := h.request("session/prompt", promptParams(sid, "b"), nil); resp.Error != nil {
		t.Fatalf("second prompt rejected: %v", resp.Error)
	}
	if asked != 1 {
		t.Fatalf("asked %d times", asked)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(h.dir, f)); err != nil {
			t.Fatalf("%s missing: %v", f, err)
		}
	}
}

func TestCancelMidTurn(t *testing.T) {
	h := newHarness(t, &scriptedChat{block: true})
	sid := h.newSession()
	h.nextID++
	id := h.nextID
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/prompt", "params": promptParams(sid, "think forever")})
	time.Sleep(100 * time.Millisecond)
	h.notify("session/cancel", map[string]any{"sessionId": sid})
	resp, _ := h.await(id, nil)
	if got := stopReason(t, resp); got != "cancelled" {
		t.Fatalf("stopReason = %s", got)
	}
}

func TestCancelWhileAwaitingPermission(t *testing.T) {
	chat := &scriptedChat{responses: []api.ChatResponse{toolCall("write_file", `{"path":"c.txt","content":"x"}`)}}
	h := newHarness(t, chat)
	sid := h.newSession()
	h.nextID++
	id := h.nextID
	h.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/prompt", "params": promptParams(sid, "w")})
	for { // wait for the permission request, then cancel instead of answering
		m := h.next()
		if m.Method == "session/request_permission" {
			break
		}
	}
	h.notify("session/cancel", map[string]any{"sessionId": sid})
	resp, _ := h.await(id, nil)
	if got := stopReason(t, resp); got != "cancelled" {
		t.Fatalf("stopReason = %s", got)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "c.txt")); err == nil {
		t.Fatal("write happened despite cancel")
	}
}

func TestMalformedJSONRPC(t *testing.T) {
	h := newHarness(t, &scriptedChat{responses: []api.ChatResponse{text("still alive")}})
	h.sendRaw(`{not json`)
	if m := h.next(); m.Error == nil || m.Error.Code != codeParseError || string(m.ID) != "null" {
		t.Fatalf("parse error response = %+v", m)
	}
	h.sendRaw(`{"jsonrpc":"2.0","id":7}`)
	if m := h.next(); m.Error == nil || m.Error.Code != codeInvalidRequest {
		t.Fatalf("invalid request response = %+v", m)
	}
	h.sendRaw(`{"jsonrpc":"2.0","id":8,"method":"nope/nothing"}`)
	if m := h.next(); m.Error == nil || m.Error.Code != codeMethodNotFound || string(m.ID) != "8" {
		t.Fatalf("method not found response = %+v", m)
	}
	h.sendRaw(`{"jsonrpc":"2.0","method":"nope/notification"}`) // no response expected
	h.sendRaw(`{"jsonrpc":"2.0","id":9,"method":"session/prompt","params":"garbage"}`)
	if m := h.next(); m.Error == nil || m.Error.Code != codeInvalidParams {
		t.Fatalf("bad params response = %+v", m)
	}
	resp, _ := h.request("session/prompt", promptParams("sess_unknown", "x"), nil)
	if resp.Error == nil || resp.Error.Code != codeInvalidParams {
		t.Fatalf("unknown session = %+v", resp)
	}
	// Unsupported content type is an error, not a crash, and the server still works.
	sid := h.newSession()
	resp, _ = h.request("session/prompt", map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "image", "data": "x", "mimeType": "image/png"}}}, nil)
	if resp.Error == nil || resp.Error.Code != codeInvalidParams {
		t.Fatalf("image prompt = %+v", resp)
	}
	resp, _ = h.request("session/prompt", promptParams(sid, "ping"), nil)
	if stopReason(t, resp) != "end_turn" {
		t.Fatalf("server did not recover: %+v", resp)
	}
	_ = h.in.Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return on EOF")
	}
}

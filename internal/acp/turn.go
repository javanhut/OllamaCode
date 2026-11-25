package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/javanhut/ollama_code/internal/agent"
	"github.com/javanhut/ollama_code/tools"
)

// turnState maps the agent loop's callbacks onto ACP notifications. The loop
// is synchronous, so every callback runs on one goroutine and the FIFO of
// announced-but-unfinished call ids needs no lock: calls are dispatched, and
// their results reported, in announcement order.
type turnState struct {
	c       *conn
	s       *session
	seq     int
	queue   []string
	started bool
}

func (t *turnState) install(o *agent.Options) {
	o.OnAssistant = t.onAssistant
	o.OnToolResult = t.onToolResult
	prevBefore := o.Before
	o.Before = func(call tools.ToolCall) {
		if prevBefore != nil {
			prevBefore(call)
		}
		if id := t.head(); id != "" {
			t.c.update(t.s.id, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": "in_progress"})
		}
	}
	o.Approve = t.approve
}

func (t *turnState) head() string {
	if len(t.queue) == 0 {
		return ""
	}
	return t.queue[0]
}

func (t *turnState) onAssistant(content string, calls []tools.ToolCall) {
	if strings.TrimSpace(content) != "" {
		t.c.update(t.s.id, map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": content},
		})
	}
	for _, call := range calls {
		t.seq++
		id := fmt.Sprintf("call_%d", t.seq)
		t.queue = append(t.queue, id)
		u := toolCallFields(call)
		u["sessionUpdate"] = "tool_call"
		u["toolCallId"] = id
		u["status"] = "pending"
		if d := t.diffContent(call); d != nil {
			u["content"] = []any{d}
		}
		t.c.update(t.s.id, u)
	}
}

func (t *turnState) onToolResult(call tools.ToolCall, result string, failed bool) {
	id := t.head()
	if id == "" {
		return
	}
	t.queue = t.queue[1:]
	status := "completed"
	if failed {
		status = "failed"
	}
	u := map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": status,
		"content": []any{textContent(resultText(result))}}
	if raw, err := json.Marshal(map[string]string{"output": result}); err == nil {
		u["rawOutput"] = json.RawMessage(raw)
	}
	t.c.update(t.s.id, u)
}

// resultText renders a tool-result envelope as readable text.
func resultText(result string) string {
	env, ok := tools.DecodeToolResult(result)
	if !ok {
		return result
	}
	parts := []string{env.Summary}
	if len(env.Evidence) > 0 {
		parts = append(parts, strings.Join(env.Evidence, "\n"))
	}
	if env.Hint != "" && !env.OK {
		parts = append(parts, env.Hint)
	}
	return strings.Join(parts, "\n")
}

func textContent(text string) map[string]any {
	return map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": text}}
}

// diffContent previews a write_file/edit_file as an ACP diff so the editor can
// render it next to the permission prompt.
func (t *turnState) diffContent(call tools.ToolCall) map[string]any {
	var a struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if json.Unmarshal(call.Function.Arguments, &a) != nil || a.Path == "" {
		return nil
	}
	abs := a.Path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(t.s.cwd, abs)
	}
	switch call.Function.Name {
	case "write_file":
		d := map[string]any{"type": "diff", "path": abs, "newText": a.Content}
		if old, err := os.ReadFile(abs); err == nil && len(old) < 1<<20 {
			d["oldText"] = string(old)
		}
		return d
	case "edit_file":
		if a.OldString != "" || a.NewString != "" {
			return map[string]any{"type": "diff", "path": abs, "oldText": a.OldString, "newText": a.NewString}
		}
	}
	return nil
}

// toolKind maps ocode tool names onto ACP ToolKind values.
func toolKind(name string) string {
	switch name {
	case "read_file", "list_directory", "get_project_tree", "file_info", "hash_file", "repo_map",
		"code_definition", "code_hover", "code_references", "disk_usage", "get_working_directory":
		return "read"
	case "write_file", "append_file", "edit_file", "multi_edit", "make_directory", "touch", "copy_file":
		return "edit"
	case "delete_file":
		return "delete"
	case "move_file":
		return "move"
	case "grep", "find_files", "find_symbol", "semantic_search", "code_index":
		return "search"
	case "run_shell", "terminal_open", "terminal_send", "job_output", "shell_output":
		return "execute"
	case "web_fetch", "web_search", "web_search_api", "web_crawl":
		return "fetch"
	}
	return "other"
}

// toolCallFields builds the title/kind/rawInput/locations common to the
// tool_call notification and the permission request's toolCall.
func toolCallFields(call tools.ToolCall) map[string]any {
	name := call.Function.Name
	var a map[string]any
	_ = json.Unmarshal(call.Function.Arguments, &a)
	str := func(k string) string { s, _ := a[k].(string); return s }
	title := name
	switch {
	case str("command") != "":
		title = name + ": " + oneLine(str("command"), 80)
	case str("path") != "":
		title = name + " " + str("path")
	case str("pattern") != "":
		title = name + " " + oneLine(str("pattern"), 60)
	case str("query") != "":
		title = name + " " + oneLine(str("query"), 60)
	case str("url") != "":
		title = name + " " + str("url")
	}
	f := map[string]any{"title": title, "kind": toolKind(name)}
	if len(call.Function.Arguments) > 0 && json.Valid(call.Function.Arguments) {
		f["rawInput"] = call.Function.Arguments
	}
	if p := str("path"); p != "" {
		f["locations"] = []any{map[string]any{"path": p}}
	}
	return f
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// approve answers an executor Ask by sending session/request_permission and
// blocking on the editor's choice.
func (t *turnState) approve(ctx context.Context, req agent.ApprovalRequest) (bool, error) {
	// Per-session memory of allow_always / reject_always. A grant never waives
	// the protected-path or untrusted-content prompts (mirrors Decide).
	t.s.mu.Lock()
	rules := append([]tools.PermissionRule(nil), t.s.rules...)
	t.s.mu.Unlock()
	if effect, ok := tools.EvaluatePermission(rules, req.Call); ok {
		if effect == tools.PermissionDeny {
			return false, nil
		}
		if effect == tools.PermissionAllow && !req.Untrusted && !req.Protected {
			return true, nil
		}
	}
	rule, scoped := tools.ScopedPermissionRuleFor(req.Call)
	options := []map[string]any{{"optionId": "allow_once", "name": "Allow once", "kind": "allow_once"}}
	if scoped && !req.Untrusted && !req.Protected {
		options = append(options, map[string]any{"optionId": "allow_always", "name": "Allow always (this session)", "kind": "allow_always"})
	}
	options = append(options, map[string]any{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"})
	if scoped {
		options = append(options, map[string]any{"optionId": "reject_always", "name": "Reject always (this session)", "kind": "reject_always"})
	}

	id := t.head()
	if id == "" {
		t.seq++
		id = fmt.Sprintf("call_%d", t.seq)
	}
	tc := toolCallFields(req.Call)
	tc["toolCallId"] = id
	if req.Reason != "" {
		tc["content"] = []any{textContent("Approval needed: " + req.Reason)}
		if d := t.diffContent(req.Call); d != nil {
			tc["content"] = []any{d, textContent("Approval needed: " + req.Reason)}
		}
	}
	reply, err := t.c.askClient(ctx, "session/request_permission", map[string]any{
		"sessionId": t.s.id, "toolCall": tc, "options": options,
	})
	if err != nil {
		return false, err
	}
	if reply.outcome.Outcome != "selected" {
		return false, nil
	}
	switch reply.outcome.OptionID {
	case "allow_once":
		return true, nil
	case "allow_always":
		if scoped && !req.Untrusted && !req.Protected {
			t.remember(rule)
		}
		return true, nil
	case "reject_always":
		if scoped {
			rule.Effect = tools.PermissionDeny
			t.remember(rule)
		}
	}
	return false, nil
}

func (t *turnState) remember(r tools.PermissionRule) {
	t.s.mu.Lock()
	t.s.rules = append(t.s.rules, r)
	t.s.mu.Unlock()
}

// askClient sends a request to the editor and waits for its response.
func (c *conn) askClient(ctx context.Context, method string, params any) (permReply, error) {
	id := c.nextID.Add(1)
	key := fmt.Sprint(id)
	ch := make(chan permReply, 1)
	c.mu.Lock()
	c.pending[key] = ch
	c.mu.Unlock()
	drop := func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}
	if err := c.w.request(id, method, params); err != nil {
		drop()
		return permReply{}, err
	}
	select {
	case r := <-ch:
		return r, r.err
	case <-ctx.Done():
		drop()
		return permReply{}, ctx.Err()
	}
}

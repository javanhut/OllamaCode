package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Tool is an Ollama/OpenAI-compatible function definition paired with a local
// handler. The Type/Function fields are what the model sees; Handler runs the
// call locally when the model emits a matching tool_call.
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
	Handler  Handler  `json:"-"`
}

type Function struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  Schema `json:"parameters"`
}

type Schema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required,omitempty"`
}

type Property struct {
	Type        string              `json:"type"`
	Description string              `json:"description,omitempty"`
	Enum        []string            `json:"enum,omitempty"`
	Items       *Property           `json:"items,omitempty"`      // element schema for type "array"
	Properties  map[string]Property `json:"properties,omitempty"` // field schemas for type "object"
	Required    []string            `json:"required,omitempty"`   // required fields for type "object"
}

// Handler executes a tool call. args is the raw JSON object the model sent;
// the returned string is fed back to the model as the tool's reply.
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

// Embedder abstracts the Ollama embedding endpoint so semantic tools can
// compute vectors without creating a circular import on api.OllamaHost.
type Embedder interface {
	Embed(model string, inputs []string) ([][]float32, error)
}

// ToolCall mirrors the shape Ollama emits inside a chat response message.
type ToolCall struct {
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Registry holds the tools available for a session.
type Registry struct {
	tools         map[string]Tool
	onFileChanged func([]string)
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// SetFileChangeHook registers a callback invoked with the affected path(s) after
// a file-mutating tool succeeds. Used to keep the semantic index fresh. The
// callback may run on a tool goroutine, so it must be concurrency-safe.
func (r *Registry) SetFileChangeHook(fn func([]string)) { r.onFileChanged = fn }

// fileMutators are the tools whose success should invalidate the semantic index.
var fileMutators = map[string]bool{
	"write_file": true, "edit_file": true, "append_file": true,
	"delete_file": true, "move_file": true, "copy_file": true, "touch": true,
}

// MutatedPaths extracts the path-like arguments of a file-mutating tool call,
// or nil if the tool doesn't mutate files. Exported for checkpoint snapshots.
func MutatedPaths(name string, raw json.RawMessage) []string {
	return mutatedPaths(name, raw)
}

// mutatedPaths extracts the path-like arguments of a file-mutating tool call.
func mutatedPaths(name string, raw json.RawMessage) []string {
	if !fileMutators[name] {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"path", "dest", "destination", "new_path", "to", "source", "src"} {
		if v, ok := m[k].(string); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (r *Registry) Register(t Tool) {
	if t.Type == "" {
		t.Type = "function"
	}
	r.tools[t.Function.Name] = t
}

// Definitions returns the tool list to send in a ChatRequest, sorted by name
// for stable output.
func (r *Registry) Definitions() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Function.Name < out[j].Function.Name })
	return out
}

// Invoke dispatches a tool call. Returns the handler's reply string or an
// error message suitable for sending back as the tool's response.
func (r *Registry) Invoke(ctx context.Context, call ToolCall) (string, error) {
	t, ok := r.tools[call.Function.Name]
	if !ok {
		if cand, d := r.Nearest(call.Function.Name); cand != "" && (d <= 3 || d <= len(call.Function.Name)/3) {
			return "", fmt.Errorf("unknown tool %q. Did you mean %q? Available tools: %s",
				call.Function.Name, cand, strings.Join(r.Names(), ", "))
		}
		return "", fmt.Errorf("unknown tool %q. Available tools: %s",
			call.Function.Name, strings.Join(r.Names(), ", "))
	}
	if t.Handler == nil {
		return "", fmt.Errorf("tool %q has no handler", call.Function.Name)
	}
	if err := ValidateArgs(t.Function, call.Function.Arguments); err != nil {
		return "", err
	}
	out, err := t.Handler(ctx, call.Function.Arguments)
	if err == nil && r.onFileChanged != nil {
		if paths := mutatedPaths(call.Function.Name, call.Function.Arguments); len(paths) > 0 {
			r.onFileChanged(paths)
		}
	}
	return out, err
}

// DefaultRegistry returns a registry pre-populated with the built-in
// filesystem and shell tools. Add more with Register.
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register(ReadFileTool())
	r.Register(WriteFileTool())
	r.Register(AppendFileTool())
	r.Register(EditFileTool())
	r.Register(DeleteFileTool())
	r.Register(MoveFileTool())
	r.Register(CopyFileTool())
	r.Register(ListDirectoryTool())
	r.Register(FindFilesTool())
	r.Register(MakeDirectoryTool())
	r.Register(TouchFileTool())
	r.Register(FileInfoTool())
	r.Register(GetWorkingDirectoryTool())
	r.Register(GrepTool())
	r.Register(RunShellTool())
	r.Register(WebFetchTool())
	r.Register(WebSearchTool())
	r.Register(GetProjectTreeTool())
	r.Register(FindSymbolTool())
	r.Register(AskUserTool())
	r.Register(GitStatusTool())
	r.Register(GitDiffTool())
	r.Register(GitLogTool())
	r.Register(GitAddTool())
	r.Register(GitCommitTool())
	r.Register(GitBranchTool())
	r.Register(GitCheckoutTool())
	r.Register(GitPullTool())
	r.Register(GitPushTool())
	r.Register(GitStashTool())
	r.Register(GitMergeTool())
	r.Register(GitResetTool())
	r.Register(GitRemoteTool())
	r.Register(GetEnvTool())
	r.Register(SetEnvTool())
	r.Register(ListEnvTool())
	r.Register(CodeDefinitionTool())
	r.Register(CodeReferencesTool())
	r.Register(CodeHoverTool())
	r.Register(WebCrawlTool())
	r.Register(WebSearchAPITool())
	r.Register(HashFileTool())
	r.Register(ProcessListTool())
	r.Register(ProcessKillTool())
	r.Register(DiskUsageTool())
	return r
}

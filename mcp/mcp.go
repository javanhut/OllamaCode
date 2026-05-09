package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// Handler executes a tool call. args is the raw JSON object the model sent;
// the returned string is fed back to the model as the tool's reply.
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

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
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
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
		return "", fmt.Errorf("unknown tool: %q", call.Function.Name)
	}
	if t.Handler == nil {
		return "", fmt.Errorf("tool %q has no handler", call.Function.Name)
	}
	return t.Handler(ctx, call.Function.Arguments)
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
	return r
}

// ----- built-in tools -----

func ReadFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "read_file",
			Description: "Read a file from disk. Optionally limit to a line range (1-indexed, inclusive). Returned text is prefixed with line numbers when a range is requested or the file has more than one line.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":       {Type: "string", Description: "Absolute or relative path to the file."},
					"start_line": {Type: "number", Description: "First line to read (1-indexed). Optional."},
					"end_line":   {Type: "number", Description: "Last line to read (1-indexed, inclusive). Optional."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path      string `json:"path"`
				StartLine int    `json:"start_line"`
				EndLine   int    `json:"end_line"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			if a.StartLine == 0 && a.EndLine == 0 {
				return string(data), nil
			}
			lines := strings.Split(string(data), "\n")
			start := a.StartLine
			end := a.EndLine
			if start < 1 {
				start = 1
			}
			if end < 1 || end > len(lines) {
				end = len(lines)
			}
			if start > end {
				return "", fmt.Errorf("start_line %d > end_line %d", start, end)
			}
			var b strings.Builder
			for i := start; i <= end; i++ {
				fmt.Fprintf(&b, "%d\t%s\n", i, lines[i-1])
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

func WriteFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "write_file",
			Description: "Write text to a file, creating it (and parent directories) if needed. Overwrites existing contents.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":    {Type: "string", Description: "Absolute or relative path to the file."},
					"content": {Type: "string", Description: "Full file contents to write."},
				},
				Required: []string{"path", "content"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(a.Path, []byte(a.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), nil
		},
	}
}

func ListDirectoryTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "list_directory",
			Description: "List the entries of a directory. Returns one entry per line, suffixing directories with '/'.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Absolute or relative path to the directory. Defaults to the current working directory."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("invalid arguments: %w", err)
				}
			}
			path := a.Path
			if path == "" {
				path = "."
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return "", err
			}
			out := make([]string, 0, len(entries))
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() {
					name += "/"
				}
				out = append(out, name)
			}
			sort.Strings(out)
			return joinLines(out), nil
		},
	}
}

func MakeDirectoryTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "make_directory",
			Description: "Create a directory and any missing parent directories (mkdir -p).",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the directory to create."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if err := os.MkdirAll(a.Path, 0o755); err != nil {
				return "", err
			}
			return "created " + a.Path, nil
		},
	}
}

func TouchFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "touch",
			Description: "Create an empty file if it does not exist, otherwise update its modification time.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to the file."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
				return "", err
			}
			now := time.Now()
			if _, err := os.Stat(a.Path); err == nil {
				if err := os.Chtimes(a.Path, now, now); err != nil {
					return "", err
				}
				return "touched " + a.Path, nil
			}
			f, err := os.OpenFile(a.Path, os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return "", err
			}
			f.Close()
			return "created " + a.Path, nil
		},
	}
}

func GrepTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "grep",
			Description: "Search for a regex pattern in files. Returns matching lines prefixed with file:line. Uses the system `grep` binary.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"pattern":   {Type: "string", Description: "Regex pattern to search for."},
					"path":      {Type: "string", Description: "File or directory to search. Defaults to '.'."},
					"recursive": {Type: "boolean", Description: "Search directories recursively. Defaults to true when path is a directory."},
					"ignore_case": {Type: "boolean", Description: "Case-insensitive match."},
				},
				Required: []string{"pattern"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Pattern    string `json:"pattern"`
				Path       string `json:"path"`
				Recursive  *bool  `json:"recursive"`
				IgnoreCase bool   `json:"ignore_case"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Pattern == "" {
				return "", fmt.Errorf("pattern is required")
			}
			path := a.Path
			if path == "" {
				path = "."
			}
			recursive := false
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				recursive = true
			}
			if a.Recursive != nil {
				recursive = *a.Recursive
			}
			argv := []string{"-nE"}
			if a.IgnoreCase {
				argv = append(argv, "-i")
			}
			if recursive {
				argv = append(argv, "-r")
			}
			argv = append(argv, "--", a.Pattern, path)
			cmd := exec.CommandContext(ctx, "grep", argv...)
			out, err := cmd.CombinedOutput()
			text := strings.TrimRight(string(out), "\n")
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
					return "no matches", nil
				}
				if text != "" {
					return text, nil
				}
				return "", err
			}
			if text == "" {
				return "no matches", nil
			}
			return text, nil
		},
	}
}

func RunShellTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "run_shell",
			Description: "Run a shell command via `bash -c`. Use this for awk, sed, find, complex pipelines, or anything not covered by a dedicated tool. Returns combined stdout+stderr; non-zero exits are reported in the result.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"command":     {Type: "string", Description: "The shell command to execute."},
					"working_dir": {Type: "string", Description: "Directory to run in. Defaults to the current working directory."},
					"timeout_sec": {Type: "number", Description: "Hard timeout in seconds. Defaults to 30."},
				},
				Required: []string{"command"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Command    string  `json:"command"`
				WorkingDir string  `json:"working_dir"`
				TimeoutSec float64 `json:"timeout_sec"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if strings.TrimSpace(a.Command) == "" {
				return "", fmt.Errorf("command is required")
			}
			timeout := 30 * time.Second
			if a.TimeoutSec > 0 {
				timeout = time.Duration(a.TimeoutSec * float64(time.Second))
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			cmd := exec.CommandContext(cctx, "bash", "-c", a.Command)
			if a.WorkingDir != "" {
				cmd.Dir = a.WorkingDir
			}
			out, err := cmd.CombinedOutput()
			text := strings.TrimRight(string(out), "\n")
			if err != nil {
				if cctx.Err() == context.DeadlineExceeded {
					return text + "\n[timed out after " + timeout.String() + "]", nil
				}
				if exitErr, ok := err.(*exec.ExitError); ok {
					return fmt.Sprintf("%s\n[exit %d]", text, exitErr.ExitCode()), nil
				}
				return "", err
			}
			if text == "" {
				return "[ok]", nil
			}
			return text, nil
		},
	}
}

func AppendFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "append_file",
			Description: "Append text to the end of a file. Creates the file (and parent directories) if missing.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":    {Type: "string", Description: "Absolute or relative path to the file."},
					"content": {Type: "string", Description: "Text to append. Add a trailing newline yourself if you want one."},
				},
				Required: []string{"path", "content"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
				return "", err
			}
			f, err := os.OpenFile(a.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return "", err
			}
			defer f.Close()
			if _, err := f.WriteString(a.Content); err != nil {
				return "", err
			}
			return fmt.Sprintf("appended %d bytes to %s", len(a.Content), a.Path), nil
		},
	}
}

func EditFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "edit_file",
			Description: "Replace an exact text snippet inside a file. By default old_string must appear exactly once (safety). Set replace_all to true to substitute every occurrence. Use this for incremental edits instead of rewriting the whole file with write_file.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":        {Type: "string", Description: "Path to the file."},
					"old_string":  {Type: "string", Description: "Exact text currently in the file. Whitespace must match."},
					"new_string":  {Type: "string", Description: "Replacement text."},
					"replace_all": {Type: "boolean", Description: "Replace every occurrence. Default false."},
				},
				Required: []string{"path", "old_string", "new_string"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if a.OldString == "" {
				return "", fmt.Errorf("old_string is required (use write_file or append_file to add new content)")
			}
			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			content := string(data)
			count := strings.Count(content, a.OldString)
			if count == 0 {
				return "", fmt.Errorf("old_string not found in %s", a.Path)
			}
			if count > 1 && !a.ReplaceAll {
				return "", fmt.Errorf("old_string appears %d times in %s; pass replace_all=true or use a more specific snippet", count, a.Path)
			}
			var updated string
			if a.ReplaceAll {
				updated = strings.ReplaceAll(content, a.OldString, a.NewString)
			} else {
				updated = strings.Replace(content, a.OldString, a.NewString, 1)
			}
			info, err := os.Stat(a.Path)
			mode := os.FileMode(0o644)
			if err == nil {
				mode = info.Mode().Perm()
			}
			if err := os.WriteFile(a.Path, []byte(updated), mode); err != nil {
				return "", err
			}
			return fmt.Sprintf("edited %s: replaced %d occurrence(s)", a.Path, count), nil
		},
	}
}

func DeleteFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "delete_file",
			Description: "Delete a file. To remove a directory and its contents, set recursive=true.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":      {Type: "string", Description: "Path to delete."},
					"recursive": {Type: "boolean", Description: "Required when deleting a non-empty directory."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path      string `json:"path"`
				Recursive bool   `json:"recursive"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			if a.Recursive {
				if err := os.RemoveAll(a.Path); err != nil {
					return "", err
				}
				return "removed " + a.Path + " (recursive)", nil
			}
			if err := os.Remove(a.Path); err != nil {
				return "", err
			}
			return "removed " + a.Path, nil
		},
	}
}

func MoveFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "move_file",
			Description: "Move or rename a file or directory.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"source":      {Type: "string", Description: "Current path."},
					"destination": {Type: "string", Description: "New path. Parent directories are created if needed."},
				},
				Required: []string{"source", "destination"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Source      string `json:"source"`
				Destination string `json:"destination"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Source == "" || a.Destination == "" {
				return "", fmt.Errorf("source and destination are required")
			}
			if err := os.MkdirAll(filepath.Dir(a.Destination), 0o755); err != nil {
				return "", err
			}
			if err := os.Rename(a.Source, a.Destination); err != nil {
				return "", err
			}
			return fmt.Sprintf("moved %s -> %s", a.Source, a.Destination), nil
		},
	}
}

func CopyFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "copy_file",
			Description: "Copy a file. Does not currently recurse into directories.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"source":      {Type: "string", Description: "Source file path."},
					"destination": {Type: "string", Description: "Destination file path. Parent directories are created if needed."},
				},
				Required: []string{"source", "destination"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Source      string `json:"source"`
				Destination string `json:"destination"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Source == "" || a.Destination == "" {
				return "", fmt.Errorf("source and destination are required")
			}
			info, err := os.Stat(a.Source)
			if err != nil {
				return "", err
			}
			if info.IsDir() {
				return "", fmt.Errorf("copy_file does not support directories; use run_shell with cp -r")
			}
			data, err := os.ReadFile(a.Source)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(a.Destination), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(a.Destination, data, info.Mode().Perm()); err != nil {
				return "", err
			}
			return fmt.Sprintf("copied %s -> %s (%d bytes)", a.Source, a.Destination, len(data)), nil
		},
	}
}

func FindFilesTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "find_files",
			Description: "Walk a directory tree and return paths whose basename matches a glob pattern (e.g. '*.go', 'README*'). Skips .git and node_modules by default.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"pattern":   {Type: "string", Description: "Glob pattern matched against the basename. Use '*' for anything."},
					"path":      {Type: "string", Description: "Root directory to search. Defaults to '.'."},
					"max_depth": {Type: "number", Description: "Maximum directory depth (0 = root only). Default 10."},
					"include_hidden": {Type: "boolean", Description: "Descend into dot-directories (default false). .git/node_modules stay skipped."},
				},
				Required: []string{"pattern"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Pattern       string `json:"pattern"`
				Path          string `json:"path"`
				MaxDepth      *int   `json:"max_depth"`
				IncludeHidden bool   `json:"include_hidden"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Pattern == "" {
				return "", fmt.Errorf("pattern is required")
			}
			root := a.Path
			if root == "" {
				root = "."
			}
			maxDepth := 10
			if a.MaxDepth != nil {
				maxDepth = *a.MaxDepth
			}
			rootDepth := strings.Count(filepath.Clean(root), string(filepath.Separator))
			var matches []string
			err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				name := info.Name()
				if name == ".git" || name == "node_modules" {
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if !a.IncludeHidden && strings.HasPrefix(name, ".") && p != root {
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				depth := strings.Count(filepath.Clean(p), string(filepath.Separator)) - rootDepth
				if depth > maxDepth {
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if !info.IsDir() {
					if ok, _ := filepath.Match(a.Pattern, name); ok {
						matches = append(matches, p)
					}
				}
				return nil
			})
			if err != nil {
				return "", err
			}
			if len(matches) == 0 {
				return "no matches", nil
			}
			sort.Strings(matches)
			return joinLines(matches), nil
		},
	}
}

func FileInfoTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "file_info",
			Description: "Return metadata for a path: type (file/dir/symlink), size, permissions, modtime. Errors if the path does not exist.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path": {Type: "string", Description: "Path to inspect."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			info, err := os.Lstat(a.Path)
			if err != nil {
				return "", err
			}
			kind := "file"
			switch {
			case info.IsDir():
				kind = "dir"
			case info.Mode()&os.ModeSymlink != 0:
				kind = "symlink"
			}
			return fmt.Sprintf("path: %s\ntype: %s\nsize: %d\nmode: %s\nmodtime: %s",
				a.Path, kind, info.Size(), info.Mode().String(), info.ModTime().Format(time.RFC3339)), nil
		},
	}
}

func GetWorkingDirectoryTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "get_working_directory",
			Description: "Return the absolute path of the process's current working directory. Use this when you need to know where relative paths resolve.",
			Parameters:  Schema{Type: "object", Properties: map[string]Property{}},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			cwd, err := os.Getwd()
			if err != nil {
				return "", err
			}
			return cwd, nil
		},
	}
}

func joinLines(s []string) string {
	if len(s) == 0 {
		return ""
	}
	out := s[0]
	for _, line := range s[1:] {
		out += "\n" + line
	}
	return out
}

package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/internal/gitignore"
)

// calculateHash computes the SHA-256 hash of a file's content.
func calculateHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func HashFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "hash_file",
			Description: "Calculate the SHA-256 hash of a file. Used for Differential State Tracking to detect drift before modification.",
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
				return "", err
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			hash, err := calculateHash(a.Path)
			if err != nil {
				return "", err
			}
			return hash, nil
		},
	}
}

func ReadFileTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "read_file",
			Description: "Read a file from disk. If the path is a directory, read every text file under it recursively (skipping VCS, build, and vendor dirs) and concatenate the results with per-file headers. Use this tool — not list_directory — whenever you actually want file contents. Optional start_line/end_line apply only to single-file reads.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":       {Type: "string", Description: "Absolute or relative path. May be a file or a directory; directories are read recursively."},
					"start_line": {Type: "number", Description: "First line to read (1-indexed). Single-file reads only."},
					"end_line":   {Type: "number", Description: "Last line to read (1-indexed, inclusive). Single-file reads only."},
				},
				Required: []string{"path"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path      string `json:"path"`
				StartLine int    `json:"start_line"`
				EndLine   int    `json:"end_line"`
				MaxBytes  int    `json:"max_bytes"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			if a.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			info, err := os.Stat(a.Path)
			if err != nil {
				return "", err
			}
			if info.IsDir() {
				if a.StartLine != 0 || a.EndLine != 0 {
					return "", fmt.Errorf("start_line/end_line are not supported when path is a directory")
				}
				return readDirRecursive(a.Path)
			}
			data, err := os.ReadFile(a.Path)
			if err != nil {
				return "", err
			}
			if a.StartLine == 0 && a.EndLine == 0 {
				maxBytes := a.MaxBytes
				if maxBytes <= 0 {
					maxBytes = 32768
				}
				// Line-number the whole-file read too (like range reads), so the
				// model always has stable coordinates to hand to edit_file. Budget is
				// applied over the emitted, numbered output.
				lines := strings.Split(string(data), "\n")
				var b strings.Builder
				used := 0
				for i, ln := range lines {
					row := fmt.Sprintf("%d\t%s\n", i+1, ln)
					if i > 0 && used+len(row) > maxBytes {
						fmt.Fprintf(&b, "... [truncated at line %d of %d; %d total bytes. Use start_line/end_line or raise max_bytes to read more.]", i, len(lines), len(data))
						return strings.TrimRight(b.String(), "\n"), nil
					}
					b.WriteString(row)
					used += len(row)
				}
				return strings.TrimRight(b.String(), "\n"), nil
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

func readDirRecursive(root string) (string, error) {
	var out strings.Builder
	var (
		filesRead    int
		filesSkipped int
		bytesRead    int
		truncated    bool
	)

	gi := gitignore.NewMatcher(root)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != root && gi.IsIgnored(path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			filesSkipped++
			return nil
		}
		// Skip dot entries (hidden files/dirs) to avoid bogging the model down
		// in config files, version control, etc.
		if name := d.Name(); strings.HasPrefix(name, ".") && path != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			filesSkipped++
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			filesSkipped++
			return nil
		}
		if bytesRead >= readDirMaxTotalBytes {
			truncated = true
			return filepath.SkipAll
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}

		f, openErr := os.Open(path)
		if openErr != nil {
			filesSkipped++
			fmt.Fprintf(&out, "===== %s =====\n[error opening: %v]\n\n", rel, openErr)
			return nil
		}

		remaining := readDirMaxTotalBytes - bytesRead
		limit := min(remaining, readDirMaxFileBytes)
		buf := make([]byte, limit+1)
		n, readErr := io.ReadFull(f, buf)
		f.Close()
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			filesSkipped++
			fmt.Fprintf(&out, "===== %s =====\n[error reading: %v]\n\n", rel, readErr)
			return nil
		}
		chunk := buf[:n]

		if isBinaryContent(chunk) {
			filesSkipped++
			fmt.Fprintf(&out, "===== %s =====\n[binary file, skipped]\n\n", rel)
			return nil
		}

		fileTruncated := n > limit
		if fileTruncated {
			chunk = chunk[:limit]
		}

		fmt.Fprintf(&out, "===== %s =====\n", rel)
		out.Write(chunk)
		if len(chunk) == 0 || chunk[len(chunk)-1] != '\n' {
			out.WriteByte('\n')
		}
		if fileTruncated {
			fmt.Fprintf(&out, "[truncated after %d bytes]\n", limit)
		}
		out.WriteByte('\n')

		filesRead++
		bytesRead += len(chunk)
		if bytesRead >= readDirMaxTotalBytes {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && err != filepath.SkipAll {
		return "", err
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Recursive read of %s — %d files read, %d skipped, %d bytes",
		root, filesRead, filesSkipped, bytesRead)
	if truncated {
		summary.WriteString(" (stopped at total byte cap)")
	}
	summary.WriteString("\n\n")
	return summary.String() + out.String(), nil
}

func isBinaryContent(b []byte) bool {
	n := min(len(b), 512)
	for i := range n {
		if b[i] == 0 {
			return true
		}
	}
	return false
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
			old, _ := os.ReadFile(a.Path) // nil for a new file
			mode := os.FileMode(0o644)
			if info, err := os.Stat(a.Path); err == nil {
				mode = info.Mode().Perm()
			}
			if err := os.WriteFile(a.Path, []byte(a.Content), mode); err != nil {
				return "", err
			}
			hash, _ := calculateHash(a.Path)
			result := fmt.Sprintf("wrote %d bytes to %s\nNew Hash: %s", len(a.Content), a.Path, hash)
			if diff := unifiedDiff(string(old), a.Content, a.Path); diff != "" {
				result += "\n" + diff
			}
			return result, nil
		},
	}
}

func ListDirectoryTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "list_directory",
			Description: "List the immediate entries of a directory (non-recursive). One entry per line; directories are suffixed with '/'. Dot entries (names starting with '.') are hidden by default. Use ONLY when the user explicitly asks to list/inspect directory structure. If you actually want file contents, call read_file on the directory — it recurses automatically.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":           {Type: "string", Description: "Absolute or relative path to the directory. Defaults to the current working directory."},
					"include_hidden": {Type: "boolean", Description: "Show dot entries (names starting with '.'). Default false."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path          string `json:"path"`
				IncludeHidden bool   `json:"include_hidden"`
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
				if !a.IncludeHidden && strings.HasPrefix(name, ".") {
					continue
				}
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

// capMatches bounds grep-style output to protect the model's context window,
// truncating to a line and byte budget with a footer noting how much was cut.
// groupMatches turns flat `path:line:content` grep output into a counted,
// file-grouped listing that's easier to scan and act on:
//
//	3 match(es) in 2 file(s):
//	a.go
//	  12: foo
//	b.go
//	  7: baz
//
// The result is passed through capMatches for the line/byte budget.
func groupMatches(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	order := []string{}
	byFile := map[string][]string{}
	total := 0
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		path := ""
		hit := "  " + ln
		if p := strings.SplitN(ln, ":", 3); len(p) == 3 {
			path = p[0]
			hit = "  " + p[1] + ": " + p[2]
		}
		if _, ok := byFile[path]; !ok {
			order = append(order, path)
		}
		byFile[path] = append(byFile[path], hit)
		total++
	}
	if total == 0 {
		return "no matches"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) in %d file(s):\n", total, len(order))
	for _, path := range order {
		if path != "" {
			b.WriteString(path + "\n")
		}
		for _, h := range byFile[path] {
			b.WriteString(h + "\n")
		}
	}
	return capMatches(strings.TrimRight(b.String(), "\n"))
}

func capMatches(text string) string {
	const maxLines, maxBytes = 100, 10 * 1024
	lines := strings.Split(text, "\n")
	truncatedBy := 0
	if len(lines) > maxLines {
		truncatedBy = len(lines) - maxLines
		lines = lines[:maxLines]
	}
	out := strings.Join(lines, "\n")
	if len(out) > maxBytes {
		out = out[:maxBytes]
		if i := strings.LastIndexByte(out, '\n'); i > 0 {
			out = out[:i]
		}
		if truncatedBy == 0 {
			truncatedBy = -1 // signal byte-based truncation
		}
	}
	switch {
	case truncatedBy > 0:
		out += fmt.Sprintf("\n\n... and %d more matching line(s) (truncated; narrow the pattern or path)", truncatedBy)
	case truncatedBy < 0:
		out += "\n\n... (truncated at 10KB; narrow the pattern or path)"
	}
	return out
}

// includeGlob turns a file_types entry into a grep --include glob. grep's
// --include matches a filename glob, so a bare extension (".go" or "go") must
// become "*.go" — passing ".go" verbatim matches only a file literally named
// ".go" and silently drops every real match. Entries that already contain glob
// metacharacters are passed through untouched.
func includeGlob(ft string) string {
	if strings.ContainsAny(ft, "*?[") {
		return ft
	}
	return "*." + strings.TrimPrefix(ft, ".")
}

func GrepTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "grep",
			Description: "Search for a regex pattern in files. Returns matching lines prefixed with file:line. ANSI color codes are stripped from output. Use this to find code patterns, usages, TODOs, or any text across the project.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"pattern":     {Type: "string", Description: "Regex pattern to search for."},
					"path":        {Type: "string", Description: "File or directory to search. Defaults to '.'."},
					"recursive":   {Type: "boolean", Description: "Search directories recursively. Defaults to true when path is a directory."},
					"ignore_case": {Type: "boolean", Description: "Case-insensitive match."},
					"file_types":  {Type: "string", Description: "Comma-separated file extensions to include (e.g. '.go,.md')."},
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
				FileTypes  string `json:"file_types"`
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
			argv := []string{"-nE", "--color=never"}
			if a.IgnoreCase {
				argv = append(argv, "-i")
			}
			if recursive {
				argv = append(argv, "-r")
			}
			// Skip dot files and dot directories so the model doesn't waste
			// context on hidden/config files unless explicitly needed.
			argv = append(argv, "--exclude-dir=.*", "--exclude=.*")
			if a.FileTypes != "" {
				for ft := range strings.SplitSeq(a.FileTypes, ",") {
					ft = strings.TrimSpace(ft)
					if ft != "" {
						argv = append(argv, "--include="+includeGlob(ft))
					}
				}
			}
			argv = append(argv, "--", a.Pattern, path)
			cmd := exec.CommandContext(ctx, "grep", argv...)
			out, err := cmd.CombinedOutput()
			text := strings.TrimRight(stripANSI(string(out)), "\n")
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
					return "no matches", nil
				}
				if text != "" {
					return groupMatches(text), nil
				}
				return "", err
			}
			if text == "" {
				return "no matches", nil
			}
			return groupMatches(text), nil
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
			f.Close()
			hash, _ := calculateHash(a.Path)
			return fmt.Sprintf("appended %d bytes to %s\nNew Hash: %s", len(a.Content), a.Path, hash), nil
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
			Description: "Copy a file or directory recursively. Uses cp -r for directories. Parent directories are created automatically for the destination.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"source":      {Type: "string", Description: "Source file or directory path."},
					"destination": {Type: "string", Description: "Destination path. Parent directories are created if needed."},
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
			cmd := exec.CommandContext(ctx, "cp", "-r", a.Source, a.Destination)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return string(out), err
			}
			return fmt.Sprintf("copied %s -> %s", a.Source, a.Destination), nil
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
					"pattern":        {Type: "string", Description: "Glob pattern matched against the basename. Use '*' for anything."},
					"path":           {Type: "string", Description: "Root directory to search. Defaults to '.'."},
					"max_depth":      {Type: "number", Description: "Maximum directory depth (0 = root only). Default 10."},
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
			gi := gitignore.NewMatcher(root)
			var matches []string
			err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				if p != root && gi.IsIgnored(p) {
					if info.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				name := info.Name()
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

func GetProjectTreeTool() Tool {
	return Tool{
		Type: "function",
		Function: Function{
			Name:        "get_project_tree",
			Description: "Return an ASCII tree of the project directory with proper branch markers (├── / └──). Directories are suffixed with '/'. Skipped dirs like .git/node_modules show a count note. Capped at max_depth and max_entries to stay readable.",
			Parameters: Schema{
				Type: "object",
				Properties: map[string]Property{
					"path":        {Type: "string", Description: "Root directory. Defaults to '.'."},
					"max_depth":   {Type: "number", Description: "Maximum directory depth. Default 4, max 8."},
					"max_entries": {Type: "number", Description: "Maximum entries to output. Default 200."},
				},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Path       string `json:"path"`
				MaxDepth   int    `json:"max_depth"`
				MaxEntries int    `json:"max_entries"`
			}
			json.Unmarshal(args, &a)
			root := a.Path
			if root == "" {
				root = "."
			}
			if a.MaxDepth <= 0 {
				a.MaxDepth = 4
			}
			if a.MaxDepth > 8 {
				a.MaxDepth = 8
			}
			if a.MaxEntries <= 0 {
				a.MaxEntries = 200
			}

			type entry struct {
				rel     string
				isDir   bool
				skipped bool
			}
			gi := gitignore.NewMatcher(root)
			var entries []entry
			skippedDirs := 0
			rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))

			filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err != nil || len(entries) >= a.MaxEntries {
					return nil
				}
				rel, _ := filepath.Rel(root, path)
				if rel == "." {
					return nil
				}
				if path != root && gi.IsIgnored(path) {
					if d.IsDir() {
						skippedDirs++
						return filepath.SkipDir
					}
					return nil
				}
				// Skip dot entries so the tree stays focused on project code.
				if name := d.Name(); strings.HasPrefix(name, ".") && path != root {
					if d.IsDir() {
						skippedDirs++
						return filepath.SkipDir
					}
					return nil
				}
				depth := strings.Count(filepath.Clean(path), string(os.PathSeparator)) - rootDepth
				if depth > a.MaxDepth {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				entries = append(entries, entry{rel: rel, isDir: d.IsDir()})
				return nil
			})

			var b strings.Builder
			b.WriteString(filepath.Clean(root))
			if !strings.HasSuffix(root, "/") {
				b.WriteByte('/')
			}
			b.WriteByte('\n')

			for i, e := range entries {
				depth := strings.Count(e.rel, string(os.PathSeparator))
				name := filepath.Base(e.rel)
				isLast := i == len(entries)-1
				if !isLast && i+1 < len(entries) {
					nextDepth := strings.Count(entries[i+1].rel, string(os.PathSeparator))
					if nextDepth < depth {
						isLast = true
					} else if nextDepth == depth {
						isLast = false
					} else {
						// Check if any later sibling at same level
						isLast = true
						for j := i + 1; j < len(entries); j++ {
							nextD := strings.Count(entries[j].rel, string(os.PathSeparator))
							if nextD < depth {
								break
							}
							if nextD == depth {
								isLast = false
								break
							}
						}
					}
				}

				// Build prefix with proper │ and ├── / └──
				parts := strings.Split(e.rel, string(os.PathSeparator))
				for d := range depth {
					// Check if the ancestor at this depth was last
					ancestorLast := true
					ancestorPath := strings.Join(parts[:d+1], string(os.PathSeparator))
					for j := i + 1; j < len(entries); j++ {
						if strings.HasPrefix(entries[j].rel, ancestorPath+string(os.PathSeparator)) {
							ancestorLast = false
							break
						}
						if !strings.HasPrefix(entries[j].rel, strings.Join(parts[:d+1], string(os.PathSeparator))) {
							break
						}
					}
					// Simpler: check if any subsequent entry is at this depth or deeper under this ancestor
					for j := i + 1; j < len(entries); j++ {
						jParts := strings.Split(entries[j].rel, string(os.PathSeparator))
						if len(jParts) <= d {
							break
						}
						if jParts[d] != parts[d] {
							break
						}
						ancestorLast = false
						break
					}
					if ancestorLast {
						b.WriteString("   ")
					} else {
						b.WriteString("│  ")
					}
				}

				if isLast {
					b.WriteString("└── ")
				} else {
					b.WriteString("├── ")
				}
				if e.isDir {
					b.WriteString(name + "/")
				} else {
					b.WriteString(name)
				}
				b.WriteByte('\n')
			}

			if skippedDirs > 0 {
				fmt.Fprintf(&b, "(%d build/vcs directories skipped)\n", skippedDirs)
			}
			if len(entries) >= a.MaxEntries {
				fmt.Fprintf(&b, "(output capped at %d entries)\n", a.MaxEntries)
			}
			fmt.Fprintf(&b, "(%d entries total)", len(entries))
			return b.String(), nil
		},
	}
}

func joinLines(s []string) string {
	return strings.Join(s, "\n")
}

// Defaults for recursive read_file on directories.
const (
	readDirMaxTotalBytes = 2 * 1024 * 1024 // 2 MiB total cap
	readDirMaxFileBytes  = 256 * 1024      // 256 KiB per file
)

// Directories skipped during recursive read_file. These are noisy build/VCS
// artifacts that almost never carry useful source context.
var readDirSkipDirs = map[string]bool{
	".git": true, ".svn": true, ".hg": true, ".bzr": true,
	"node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, "out": true, "bin": true, "obj": true,
	"__pycache__": true, ".venv": true, "venv": true,
	".idea": true, ".vscode": true,
	".next": true, ".nuxt": true,
	"coverage": true, ".cache": true, ".terraform": true,
}

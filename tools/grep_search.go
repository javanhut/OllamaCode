package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/javanhut/ollama_code/internal/gitignore"
)

// grepQuery is a normalized grep tool call.
type grepQuery struct {
	Pattern    string
	Path       string
	Recursive  bool
	IgnoreCase bool
	Globs      []string // include globs, e.g. "*.go"
	Mode       string   // "content", "files", or "count"
	Context    int      // lines of context around each match (content mode)
}

var (
	rgOnce sync.Once
	rgPath string
)

// ripgrepBinary returns the rg path, or "" when it isn't installed. Resolved
// once: PATH doesn't change under a running session.
func ripgrepBinary() string {
	rgOnce.Do(func() {
		rgPath, _ = exec.LookPath("rg")
	})
	return rgPath
}

// runGrepSearch searches with ripgrep when available, else GNU grep. ripgrep
// honors .gitignore, so node_modules, vendor and build output stay out of the
// model's context; the grep fallback approximates that with the same skip
// list and ignore matcher find_files uses. Returns raw `path:line:text` style
// output, "" for no matches.
func runGrepSearch(ctx context.Context, q grepQuery) (string, error) {
	if rg := ripgrepBinary(); rg != "" {
		return runRipgrep(ctx, rg, q)
	}
	return runGNUGrep(ctx, q)
}

func runRipgrep(ctx context.Context, rg string, q grepQuery) (string, error) {
	argv := []string{"--no-heading", "--color=never", "--no-config",
		// .gitignore applies in ivaldi repos too, not only inside a git checkout.
		"--no-require-git"}
	if ig := filepath.Join(WorkspaceRoot(), ".ivaldiignore"); fileExists(ig) {
		argv = append(argv, "--ignore-file", ig)
	}
	switch q.Mode {
	case "files":
		argv = append(argv, "-l")
	case "count":
		argv = append(argv, "-c", "--with-filename")
	default:
		argv = append(argv, "-n")
		if q.Context > 0 {
			argv = append(argv, "-C", strconv.Itoa(q.Context))
		}
	}
	if q.IgnoreCase {
		argv = append(argv, "-i")
	}
	if !q.Recursive {
		argv = append(argv, "--max-depth", "1")
	}
	for _, g := range q.Globs {
		argv = append(argv, "-g", g)
	}
	// rg skips only what the ignore files list; a repo that never listed
	// node_modules would still flood the output. Apply the same skip list as
	// the grep fallback, unless the search names a skipped dir explicitly.
	if q.Recursive && !pathInSkippedDir(q.Path) {
		for dir := range gitignore.DefaultSkipDirs {
			argv = append(argv, "-g", "!"+dir+"/")
		}
	}
	argv = append(argv, "-e", q.Pattern, "--", q.Path)
	out, err := exec.CommandContext(ctx, rg, argv...).CombinedOutput()
	text := strings.TrimRight(stripANSI(string(out)), "\n")
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
		return "", nil // rg: no matches
	}
	if err != nil && text == "" {
		return "", err
	}
	return text, nil
}

func runGNUGrep(ctx context.Context, q grepQuery) (string, error) {
	argv := []string{"-E", "--color=never"}
	switch q.Mode {
	case "files":
		argv = append(argv, "-l")
	case "count":
		argv = append(argv, "-c")
	default:
		argv = append(argv, "-n")
		if q.Context > 0 {
			argv = append(argv, "-C", strconv.Itoa(q.Context))
		}
	}
	if q.IgnoreCase {
		argv = append(argv, "-i")
	}
	if q.Recursive {
		argv = append(argv, "-r")
	}
	// Skip dot files and dot directories so the model doesn't waste
	// context on hidden/config files unless explicitly needed. The glob is
	// .?* rather than .*: some greps (ugrep) test the command-line root
	// against --exclude-dir too, and ".*" matches "." itself, which made
	// every search of the default path return nothing.
	argv = append(argv, "--exclude-dir=.?*", "--exclude=.?*")
	// An explicitly named ignored dir (grep build/) is still searched.
	filterIgnored := q.Recursive && !pathInSkippedDir(q.Path)
	if filterIgnored {
		for dir := range gitignore.DefaultSkipDirs {
			if !strings.HasPrefix(dir, ".") {
				argv = append(argv, "--exclude-dir="+dir)
			}
		}
	}
	for _, g := range q.Globs {
		argv = append(argv, "--include="+g)
	}
	// Clean turns "./" into ".", which the exclude globs above can't match.
	argv = append(argv, "--", q.Pattern, filepath.Clean(q.Path))
	out, err := exec.CommandContext(ctx, "grep", argv...).CombinedOutput()
	text := strings.TrimRight(stripANSI(string(out)), "\n")
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
		return "", nil
	}
	if err != nil && text == "" {
		return "", err
	}
	if filterIgnored {
		text = dropGitignored(text)
	}
	if q.Mode == "count" {
		text = dropZeroCounts(text)
	}
	return text, nil
}

// pathInSkippedDir reports whether p names or lies inside a directory the
// skip list excludes, e.g. node_modules/pkg. Those searches are explicit, so
// the excludes must not apply to them.
func pathInSkippedDir(p string) bool {
	for part := range strings.SplitSeq(filepath.ToSlash(filepath.Clean(p)), "/") {
		if gitignore.DefaultSkipDirs[part] {
			return true
		}
	}
	return false
}

// dropGitignored removes grep output lines whose file the workspace
// .gitignore excludes. Lines without a path prefix (context separators) are
// kept.
func dropGitignored(text string) string {
	if text == "" {
		return text
	}
	root := WorkspaceRoot()
	gi := gitignore.NewMatcher(root)
	var kept []string
	for ln := range strings.SplitSeq(text, "\n") {
		path := grepLinePath(ln)
		if path == "" {
			kept = append(kept, ln)
			continue
		}
		abs := path
		if !filepath.IsAbs(abs) {
			if cwd, err := os.Getwd(); err == nil {
				abs = filepath.Join(cwd, abs)
			}
		}
		if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") && gi.IsIgnored(rel) {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

// grepLinePath returns the file a grep output line belongs to: the whole
// line in -l mode, else the shortest ":" or "-" prefix that names an existing
// file (file names may themselves contain dashes). "" when none does.
func grepLinePath(ln string) string {
	if fileExists(ln) {
		return ln
	}
	for i := 0; i < len(ln); i++ {
		if (ln[i] == ':' || ln[i] == '-') && i > 0 && fileExists(ln[:i]) {
			return ln[:i]
		}
	}
	return ""
}

// dropZeroCounts removes "path:0" lines, which grep -c prints for every file
// it searched.
func dropZeroCounts(text string) string {
	var kept []string
	for ln := range strings.SplitSeq(text, "\n") {
		if strings.HasSuffix(ln, ":0") || ln == "0" {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

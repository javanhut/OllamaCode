// Package instructions loads the user's standing rules for a workspace from
// AGENTS.md-style files, the convention opencode, Codex and Claude Code share,
// so a repository that already carries one works here unchanged.
//
// Three sources, broad to specific:
//
//  1. the global file, <config dir>/ollama_code/AGENTS.md;
//  2. extra paths the user listed in config ("instructions": [...]);
//  3. one file per directory from the repository root down to the working
//     directory, picking the first of FileNames present in each.
//
// Files that live deeper than the working directory are picked up lazily, the
// first time a tool touches a path beneath them (see Tracker.ForPath), because
// loading every nested AGENTS.md up front spends context on subtrees the task
// never visits.
package instructions

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FileNames are the per-directory instruction files, in priority order. Only
// the first one present in a directory is loaded: a repo that ships both an
// AGENTS.md and a CLAUDE.md symlinked to it should not pay for it twice.
var FileNames = []string{"AGENTS.md", "OLLAMA.md", "CLAUDE.md"}

// DefaultMaxBytes caps the combined size of the loaded files. Instructions ride
// the cached system prefix on every request, and a small local model's context
// is a few thousand tokens — a sprawling rules file must not crowd out the task.
const DefaultMaxBytes = 32 * 1024

// File is one loaded instruction file.
type File struct {
	Path    string
	Content string
}

// Options configures Load. Zero values mean defaults.
type Options struct {
	Cwd        string   // working directory; "" = os.Getwd
	GlobalDir  string   // directory holding the global AGENTS.md; "" = <UserConfigDir>/ollama_code
	Extra      []string // extra files from config; "~/" is expanded, relative paths resolve against Cwd
	MaxBytes   int      // combined budget; 0 = DefaultMaxBytes
	NoGlobal   bool     // skip the global file (tests)
	HomeDirFor string   // override for "~" expansion (tests); "" = os.UserHomeDir
}

// Set is the result of Load.
type Set struct {
	Files   []File
	Dropped []string // paths dropped or truncated to fit the budget
	Root    string   // repository root the project walk started from
}

// Load reads every applicable instruction file. Missing files are not errors;
// unreadable ones are skipped silently, since a rules file the harness cannot
// open is one the user did not mean to hand it.
func Load(opts Options) Set {
	cwd := opts.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwd = absClean(cwd)
	budget := opts.MaxBytes
	if budget <= 0 {
		budget = DefaultMaxBytes
	}

	var files []File
	seenPath := map[string]bool{}
	seenBody := map[string]bool{}
	add := func(path string) {
		path = absClean(path)
		real := path
		if r, err := filepath.EvalSymlinks(path); err == nil {
			real = r
		}
		if seenPath[real] {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			return
		}
		seenPath[real] = true
		// Identical text under two names (AGENTS.md copied to CLAUDE.md one
		// level up) is included once.
		if seenBody[string(data)] {
			return
		}
		seenBody[string(data)] = true
		files = append(files, File{Path: path, Content: string(data)})
	}

	if !opts.NoGlobal {
		dir := opts.GlobalDir
		if dir == "" {
			if base, err := os.UserConfigDir(); err == nil {
				dir = filepath.Join(base, "ollama_code")
			}
		}
		if dir != "" {
			add(filepath.Join(dir, "AGENTS.md"))
		}
	}
	for _, p := range opts.Extra {
		if p = expandHome(strings.TrimSpace(p), opts.HomeDirFor); p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		add(p)
	}
	root := RepoRoot(cwd)
	for _, dir := range dirsBetween(root, cwd) {
		if f := firstInstructionFile(dir); f != "" {
			add(f)
		}
	}

	set := Set{Root: root}
	set.Files, set.Dropped = fit(files, budget)
	return set
}

// fit enforces the byte budget. Broad files go first — the most specific file
// is the one most likely to describe the code actually being worked on — and if
// the survivor alone still overflows, it is truncated with a visible marker
// rather than dropped, so the model knows it is reading a partial rule set.
func fit(files []File, budget int) ([]File, []string) {
	total := 0
	for _, f := range files {
		total += len(f.Content)
	}
	var dropped []string
	for total > budget && len(files) > 1 {
		dropped = append(dropped, files[0].Path)
		total -= len(files[0].Content)
		files = files[1:]
	}
	if total > budget && len(files) == 1 {
		f := files[0]
		cut := budget
		for cut > 0 && cut < len(f.Content) && !isRuneStart(f.Content[cut]) {
			cut--
		}
		f.Content = f.Content[:cut] + fmt.Sprintf("\n\n[... truncated: %s is %d bytes, over the %d-byte instruction budget ...]", f.Path, total, budget)
		files = []File{f}
		dropped = append(dropped, f.Path+" (truncated)")
	}
	return files, dropped
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Render formats the set for the system prompt. Empty when nothing loaded.
func (s Set) Render() string {
	if len(s.Files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n# Project instructions\n")
	b.WriteString("The user wrote the following instruction files for this workspace. Follow them: they take precedence over your default style and workflow preferences, but NOT over the current mode's rules, permission prompts, or safety rules.\n")
	for _, f := range s.Files {
		b.WriteString(RenderFile(f))
	}
	return b.String()
}

// RenderFile formats a single file under its path header.
func RenderFile(f File) string {
	return "\n## Instructions from: " + f.Path + "\n" + f.Content + "\n"
}

// Paths lists the loaded file paths, broad to specific.
func (s Set) Paths() []string {
	out := make([]string, len(s.Files))
	for i, f := range s.Files {
		out[i] = f.Path
	}
	return out
}

// RepoRoot returns the nearest ancestor of dir holding a .git or .ivaldi
// marker, or dir itself when there is none. $HOME is never a root on account
// of ~/.ivaldi, which is ivaldi's global config directory, not a repository
// (the same trap tools.detectVCS documents).
func RepoRoot(dir string) string {
	dir = absClean(dir)
	home, _ := os.UserHomeDir()
	for cur := dir; ; {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		if cur != home {
			if st, err := os.Stat(filepath.Join(cur, ".ivaldi")); err == nil && st.IsDir() {
				return cur
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return dir
		}
		cur = parent
	}
}

// dirsBetween lists root, then each directory down to leaf. leaf must be root
// or beneath it; otherwise only leaf is returned.
func dirsBetween(root, leaf string) []string {
	rel, err := filepath.Rel(root, leaf)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return []string{leaf}
	}
	out := []string{root}
	if rel == "." {
		return out
	}
	cur := root
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		out = append(out, cur)
	}
	return out
}

func firstInstructionFile(dir string) string {
	for _, name := range FileNames {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func absClean(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}

func expandHome(p, home string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		if home == "" {
			return ""
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// Tracker attaches instruction files from directories deeper than the working
// directory the first time a tool touches a path beneath them. It remembers
// what the system prompt already carries so nothing is sent twice.
type Tracker struct {
	mu     sync.Mutex
	cwd    string
	loaded map[string]bool // cleaned absolute file paths already in context
	budget int             // bytes still available for lazy files
}

// NewTracker seeds a tracker with the files Load already put in the prompt.
func NewTracker(cwd string, set Set) *Tracker {
	t := &Tracker{cwd: absClean(cwd), loaded: map[string]bool{}, budget: DefaultMaxBytes}
	for _, f := range set.Files {
		t.loaded[f.Path] = true
		t.budget -= len(f.Content)
	}
	return t
}

// ForPath returns the rendered text of any not-yet-loaded instruction files in
// directories between the working directory (exclusive) and the directory of
// target (inclusive), or "" when there are none. Paths outside the working
// directory are ignored: their rules belong to some other project.
func (t *Tracker) ForPath(target string) string {
	if t == nil || target == "" {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(t.cwd, target)
	}
	target = filepath.Clean(target)
	dir := target
	if st, err := os.Stat(target); err != nil || !st.IsDir() {
		dir = filepath.Dir(target)
	}
	rel, err := filepath.Rel(t.cwd, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var b strings.Builder
	for _, d := range dirsBetween(t.cwd, dir)[1:] {
		f := firstInstructionFile(d)
		if f == "" || t.loaded[f] {
			continue
		}
		t.loaded[f] = true
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		body := strings.TrimSpace(string(data))
		if body == "" || len(body) > t.budget {
			continue
		}
		t.budget -= len(body)
		b.WriteString(RenderFile(File{Path: f, Content: body}))
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n[Instruction files for this directory — rules the user wrote; follow them for work under it:]" + b.String()
}

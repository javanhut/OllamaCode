package tui

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/javanhut/ollama_code/internal/safeshell"
)

// customCommand is a user-defined slash command: a markdown file whose body is
// a prompt template, in the format opencode and Claude Code already use, so a
// command written for either works here unchanged.
//
//	---
//	description: Review the staged diff
//	mode: explore
//	---
//	Review the staged changes for $ARGUMENTS. Current diff:
//	!`git diff --cached`
//
// Placeholders: $ARGUMENTS is the raw argument string; $1..$9 are positional
// (quotes group words), and the highest-numbered one used swallows the rest.
// A template with no placeholder gets the arguments appended. !`cmd` is
// replaced by the command's output, run in the working directory. @path
// mentions are attached exactly as in a typed message.
type customCommand struct {
	name        string // including the leading slash
	description string
	mode        string // optional mode to switch to before running
	template    string
	path        string
	global      bool // from the user's config dir, not the repository
}

// customCommandDirs lists where command files live, lowest precedence first:
// the global directory, then the project's. A project command overrides a
// global one of the same name.
func customCommandDirs() []string {
	var dirs []string
	if base, err := os.UserConfigDir(); err == nil {
		dirs = append(dirs, filepath.Join(base, "ollama_code", "commands"))
	}
	root := workspaceRoot()
	dirs = append(dirs,
		filepath.Join(root, ".opencode", "command"),
		filepath.Join(root, ".opencode", "commands"),
		filepath.Join(root, ".claude", "commands"),
		filepath.Join(root, ".ollama_code", "commands"),
	)
	return dirs
}

// loadCustomCommands reads every command file. Nested files are namespaced by
// directory with ":" (commands/git/pr.md → /git:pr). A name that collides with
// a built-in is skipped: built-ins are the harness's safety surface (/undo,
// /mode) and a file must not be able to shadow them.
func loadCustomCommands(dirs []string) []customCommand {
	byName := map[string]customCommand{}
	globalDir := ""
	if base, err := os.UserConfigDir(); err == nil {
		globalDir = filepath.Join(base, "ollama_code", "commands")
	}
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return nil
			}
			name := "/" + strings.ReplaceAll(strings.TrimSuffix(rel, filepath.Ext(rel)), string(filepath.Separator), ":")
			if strings.ContainsAny(name, " \t\n") || isBuiltinSlashCommand(name) {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			cmd := parseCustomCommand(string(data))
			cmd.name, cmd.path, cmd.global = name, path, dir == globalDir
			if strings.TrimSpace(cmd.template) == "" {
				return nil
			}
			byName[name] = cmd
			return nil
		})
	}
	out := make([]customCommand, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// parseCustomCommand splits optional YAML-ish frontmatter from the template.
// Only flat "key: value" lines are understood — enough for description and
// mode, without a YAML dependency. Unknown keys (agent, model, subtask from
// other harnesses) are ignored rather than rejected, so shared files load.
func parseCustomCommand(src string) customCommand {
	var c customCommand
	src = strings.TrimPrefix(src, "\ufeff")
	body := src
	if strings.HasPrefix(src, "---\n") || strings.HasPrefix(src, "---\r\n") {
		rest := src[strings.Index(src, "\n")+1:]
		if end := strings.Index(rest, "\n---"); end >= 0 {
			front := rest[:end]
			body = rest[end+4:]
			if nl := strings.Index(body, "\n"); nl >= 0 {
				body = body[nl+1:]
			} else {
				body = ""
			}
			for _, line := range strings.Split(front, "\n") {
				key, val, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				val = strings.Trim(strings.TrimSpace(val), `"'`)
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "description":
					c.description = val
				case "mode":
					c.mode = strings.ToLower(val)
				}
			}
		}
	}
	c.template = strings.TrimSpace(body)
	if c.description == "" {
		// First line of the body, the way a commit subject describes a commit.
		first, _, _ := strings.Cut(c.template, "\n")
		c.description = truncateRunes(strings.TrimSpace(first), 60)
	}
	return c
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

var positionalArg = regexp.MustCompile(`\$([1-9])`)

// splitCommandArgs splits on whitespace, keeping "quoted groups" together.
func splitCommandArgs(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// expandTemplate substitutes the argument placeholders.
func expandTemplate(tmpl, rawArgs string) string {
	args := splitCommandArgs(rawArgs)
	highest := 0
	for _, m := range positionalArg.FindAllStringSubmatch(tmpl, -1) {
		if n, _ := strconv.Atoi(m[1]); n > highest {
			highest = n
		}
	}
	hasArguments := strings.Contains(tmpl, "$ARGUMENTS")
	out := positionalArg.ReplaceAllStringFunc(tmpl, func(tok string) string {
		n, _ := strconv.Atoi(tok[1:])
		if n > len(args) {
			return ""
		}
		if n == highest {
			return strings.Join(args[n-1:], " ")
		}
		return args[n-1]
	})
	out = strings.ReplaceAll(out, "$ARGUMENTS", rawArgs)
	if highest == 0 && !hasArguments && strings.TrimSpace(rawArgs) != "" {
		out += "\n\n" + rawArgs
	}
	return out
}

var shellInterpolation = regexp.MustCompile("!`([^`\n]+)`")

// commandShellTimeout bounds each !`cmd` so a hung command cannot freeze the
// input box: expansion runs on the update goroutine.
const commandShellTimeout = 10 * time.Second

// expandShell replaces each !`cmd` with that command's combined output, capped
// so one `cat` of a huge file cannot blow the context. A global command file
// is the user's own, so it runs with the user's authority. A repository's
// command file is not: a cloned repo could ship a /review whose expansion is
// `curl … | sh`, and typing /review is not consent to that. Those expansions
// run only when they pass the explore-mode read-only allowlist (git diff,
// ls, cat, …); anything else is left unexpanded with a note.
func expandShell(tmpl string, trusted bool) string {
	return shellInterpolation.ReplaceAllStringFunc(tmpl, func(tok string) string {
		command := shellInterpolation.FindStringSubmatch(tok)[1]
		if !trusted {
			if ok, reason := safeshell.IsExploreReadOnlyShell(command); !ok {
				return fmt.Sprintf("%s [not run: repository command files may only inline read-only commands (%s); move the command to the global commands directory to allow it]", tok, reason)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), commandShellTimeout)
		defer cancel()
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd := exec.CommandContext(ctx, shell, "-c", command)
		cmd.Dir = workspaceRoot()
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		err := cmd.Run()
		out := strings.TrimRight(buf.String(), "\n")
		const maxOut = 16 * 1024
		if len(out) > maxOut {
			out = out[:maxOut] + "\n[... output truncated ...]"
		}
		if err != nil {
			out += fmt.Sprintf("\n[command `%s` failed: %v]", command, err)
		}
		return out
	})
}

// findCustomCommand resolves "/name args..." to a command and its arguments.
func (m *Model) findCustomCommand(val string) (customCommand, string, bool) {
	name, args, _ := strings.Cut(val, " ")
	for _, c := range m.customCommands {
		if c.name == name {
			return c, strings.TrimSpace(args), true
		}
	}
	return customCommand{}, "", false
}

// runCustomCommand expands the command into the input box's text; the caller
// then submits it as an ordinary message, so mentions, queueing and history
// all behave exactly as if the user had typed the expansion.
func (m *Model) runCustomCommand(c customCommand, args string) string {
	if c.mode != "" {
		if target, ok := parseMode(c.mode); ok && target != m.mode {
			m.applyModeTransition(target, "requested by "+c.name)
		}
	}
	return expandShell(expandTemplate(c.template, args), c.global)
}

// initPrompt is the built-in /init command: generate or refresh AGENTS.md.
const initPrompt = `Create or update the AGENTS.md file at the root of this repository. It is loaded into every future session as standing instructions, so it must be short, accurate, and specific to THIS codebase.

First investigate: read the README, build files (Makefile, go.mod, package.json, Cargo.toml, pyproject.toml, …), CI config, any existing AGENTS.md / CLAUDE.md / .cursorrules / .github/copilot-instructions.md, and a few representative source files.

Then write AGENTS.md (about 20–40 lines) covering:
- Build, lint, and test commands — including how to run a SINGLE test.
- Code style that differs from language defaults: imports, formatting, naming, error handling, comment conventions.
- Architecture notes a newcomer would get wrong: package layout, where things live, invariants.
- Anything surprising (generated files not to edit, required env vars, platform quirks).

Do not include generic advice ("write clean code", "add tests"). Do not invent commands you did not see evidence for. If AGENTS.md already exists, improve it in place rather than replacing good content.`

// initCommand runs /init: it needs to write a file, so it moves to write mode
// (each write still prompts), and the reload in submit picks the new file up
// on the next turn.
func (m *Model) initCommand(extra string) string {
	if m.mode == ExploreMode || m.mode == PlanMode {
		m.applyModeTransition(WriteMode, "/init writes AGENTS.md")
	}
	if strings.TrimSpace(extra) != "" {
		return initPrompt + "\n\nAdditional guidance from the user: " + extra
	}
	return initPrompt
}

// customCommandsReport is the /commands output.
func (m *Model) customCommandsReport() string {
	if len(m.customCommands) == 0 {
		var b strings.Builder
		b.WriteString("No custom commands. Add markdown files to any of:\n")
		for _, d := range customCommandDirs() {
			b.WriteString("  " + d + "\n")
		}
		b.WriteString("\nThe file name is the command (review.md → /review); the body is the prompt. Use $ARGUMENTS or $1..$9 for arguments and !`cmd` to inline a command's output.")
		return b.String()
	}
	var b strings.Builder
	b.WriteString("Custom commands:\n")
	for _, c := range m.customCommands {
		fmt.Fprintf(&b, "  %-20s %s  (%s)\n", c.name, c.description, c.path)
	}
	return b.String()
}

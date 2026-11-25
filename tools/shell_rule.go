package tools

import (
	"slices"
	"strings"

	"github.com/javanhut/ollama_code/internal/safeshell"
)

// commandArity says how many leading non-flag words name "the command" a user
// means when they approve one: approving `git status` should not approve
// `git push --force`, and approving `npm run test` should not approve
// `npm run deploy`. Longest matching prefix wins; a program not listed widens
// to its first word. Adapted from opencode's permission/arity.ts.
var commandArity = map[string]int{
	"bun": 2, "bun run": 3, "bun x": 3, "bunx": 2,
	"cargo": 2, "cargo run": 3, "cargo add": 3,
	"deno": 2, "deno task": 3,
	"docker": 2, "docker compose": 3, "docker container": 3, "docker image": 3, "docker volume": 3, "docker network": 3,
	"dotnet": 2,
	"gh":     3,
	"git":    2, "git config": 3, "git remote": 3, "git stash": 3,
	"go": 2, "go mod": 3,
	"gradle": 2, "helm": 2, "just": 2,
	"kubectl": 2, "kubectl rollout": 3,
	"make": 2, "mvn": 2,
	"npm": 2, "npm run": 3, "npm exec": 3,
	"npx": 2, "nx": 2,
	"pip": 2, "pip3": 2, "pipx": 2, "pnpm": 2, "pnpm run": 3, "pnpm exec": 3, "poetry": 2, "poetry run": 3,
	"python -m": 3, "python3 -m": 3,
	"rustup": 2, "systemctl": 2, "terraform": 2,
	"uv": 2, "uv run": 3, "uv pip": 3,
	"yarn": 2, "yarn run": 3,
}

// ShellRulePrefix returns the approval prefix for a command line, e.g.
// "git status" for "git status --short". Only the first simple command is
// considered: that is the command the user is looking at. Git's global
// options are skipped, so `git -C sub status` is still "git status".
func ShellRulePrefix(command string) string {
	var argv []string
	for _, c := range safeshell.Parse(command).Commands {
		if len(c.Argv) > 0 && !c.Inner {
			argv = safeshell.GitStripGlobals(c.Argv, false)
			break
		}
	}
	var words []string
	for i, f := range argv {
		// "-m" is kept because "python -m pytest" names a module runner; other
		// flags never count as command words.
		if strings.HasPrefix(f, "-") && !(f == "-m" && i == 1) {
			continue
		}
		words = append(words, f)
	}
	if len(words) == 0 {
		return ""
	}
	for n := len(words); n > 0; n-- {
		if arity, ok := commandArity[strings.Join(words[:n], " ")]; ok {
			return strings.Join(words[:min(arity, len(words))], " ")
		}
	}
	return words[0]
}

// matchShellRule decides whether a command-scoped rule covers a command line.
// The line is parsed into an AST (safeshell.Parse) and each simple command's
// argv is checked; nothing is split on strings.
//
// Deny and ask must catch the dangerous command wherever it hides, so they
// match when ANY simple command the parser could extract matches — including
// commands inside substitutions, subshells, `sh -c` strings and wrappers such
// as `env`/`sudo` — even when other parts of the line were not understood.
//
// Allow must not be smuggled past: "allow git status *" may not cover
// `git status && curl … | sh`, so the line must be fully understood and EVERY
// simple command must be covered, either by the rule itself or by being a
// read-only command explore mode already permits (so `go test ./... | tail
// -20` still matches "go test *"). A command only counts as covered by the
// rule when its assignments and redirections are also harmless: `git status >
// ~/.bashrc` and `PATH=/tmp/x git status` are not `git status`. Anything the
// parser flags as complex never matches an allow rule, so it prompts.
func matchShellRule(rule PermissionRule, command string) bool {
	pattern := strings.TrimSpace(rule.Cmd)
	p := safeshell.Parse(command)
	if rule.Effect != PermissionAllow {
		if matchCommand(pattern, command) {
			return true
		}
		for _, c := range p.Commands {
			for _, t := range c.MatchTexts(true) {
				if matchCommand(pattern, t) {
					return true
				}
			}
		}
		return false
	}
	if p.Complex || len(p.Commands) == 0 {
		return false
	}
	matchedRule := false
	for _, c := range p.Commands {
		if p.SetsVars && slices.Contains(c.Dyn, true) {
			return false
		}
		if len(c.Argv) > 0 && commandMatchesRule(pattern, c) {
			matchedRule = true
			continue
		}
		if ok, _ := safeshell.CommandReadOnly(c); ok {
			continue
		}
		return false
	}
	return matchedRule
}

func commandMatchesRule(pattern string, c safeshell.Command) bool {
	if !safeshell.AssignsSafe(c) || !safeshell.RedirectsBenign(c) {
		return false
	}
	for _, t := range c.MatchTexts(false) {
		if matchCommand(pattern, t) {
			return true
		}
	}
	return false
}

// ShellMayMutate reports whether call runs shell input that could change files
// outside the file tools' view: `sed -i`, `rm`, a code generator. MutatedPaths
// can't name those paths, so callers snapshot for undo and check the
// workspace afterwards instead. A command that passes the explore-mode
// read-only allowlist is exempt, so `ls` and `git status` stay free.
func ShellMayMutate(call ToolCall) bool {
	switch call.Function.Name {
	case "terminal_send":
		return true
	case "run_shell":
		cmd := safeshell.ExtractShellCommand(call.Function.Arguments)
		ok, _ := safeshell.IsExploreReadOnlyShell(cmd)
		return !ok
	}
	return false
}

package tools

import (
	"regexp"
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
// "git status" for "git status --short". Only the first pipeline segment is
// considered: that is the command the user is looking at.
func ShellRulePrefix(command string) string {
	segs := shellRuleSegments(command)
	if len(segs) == 0 {
		return ""
	}
	fields := strings.Fields(segs[0])
	// Leading VAR=value assignments are environment, not the command.
	for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") {
		fields = fields[1:]
	}
	var words []string
	for i, f := range fields {
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

// fdRedirect matches descriptor duplications (2>&1, >&2, &>file) so the lone
// '&' inside them is not mistaken for a background operator.
var fdRedirect = regexp.MustCompile(`\d*>&\d*|&>>?`)

// shellRuleSegments splits a command line into the simple commands it runs:
// on |, ||, &&, ;, newlines and a background &.
func shellRuleSegments(command string) []string {
	var out []string
	for _, line := range strings.Split(command, "\n") {
		for _, seg := range safeshell.SplitShellSegments(line) {
			for _, part := range strings.Split(fdRedirect.ReplaceAllString(seg, " "), "&") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
		}
	}
	return out
}

// matchShellRule decides whether a command-scoped rule covers a command line.
//
// Deny and ask must catch the dangerous command wherever it hides, so they
// match when ANY simple command in the line matches. Allow must not be
// smuggled past: "allow git status *" may not cover
// `git status && curl … | sh`, so EVERY simple command must be covered —
// either by the rule itself or by being a read-only command explore mode
// already permits (so `go test ./... | tail -20` still matches "go test *").
// Command substitution hides a command from this analysis entirely, so an
// allow rule never matches a line that uses it.
func matchShellRule(rule PermissionRule, command string) bool {
	pattern := strings.TrimSpace(rule.Cmd)
	if rule.Effect != PermissionAllow {
		if matchCommand(pattern, command) {
			return true
		}
		for _, seg := range shellRuleSegments(command) {
			if matchCommand(pattern, seg) {
				return true
			}
		}
		return false
	}
	if strings.Contains(command, "`") || strings.Contains(command, "$(") || strings.Contains(command, "<(") || strings.Contains(command, ">(") {
		return false
	}
	segs := shellRuleSegments(command)
	if len(segs) == 0 {
		return false
	}
	matchedRule := false
	for _, seg := range segs {
		if matchCommand(pattern, seg) {
			matchedRule = true
			continue
		}
		if ok, _ := safeshell.IsExploreReadOnlyShell(seg); ok {
			continue
		}
		return false
	}
	return matchedRule
}

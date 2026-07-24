// Package safeshell decides which shell commands a model may run in explore
// mode (read-only allowlist) and vets commands that try to bypass the git_*
// tools with bare `git` invocations in an ivaldi repo.
package safeshell

import (
	"encoding/json"
	"fmt"
	"strings"
)

// exploreShellAllowedBins is the read-only allowlist for run_shell in explore
// mode. The check is per-segment (commands split on |, ||, &&, ;) and matches
// the first non-env-assignment token. Anything not in this list is rejected
// with a hint to switch to write mode.
var exploreShellAllowedBins = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true,
	"pwd": true, "echo": true, "printf": true,
	"wc": true, "file": true, "stat": true,
	"du": true, "df": true, "free": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true,
	"find": true, "fd": true, "tree": true,
	"which": true, "whereis": true, "type": true, "command": true,
	"ps": true, "uptime": true, "whoami": true, "id": true,
	"uname": true, "hostname": true, "date": true,
	"env": true, "printenv": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "column": true,
	"true": true, "false": true,
	"basename": true, "dirname": true, "realpath": true, "readlink": true,
	"go":     true,
	"git":    true,
	"ivaldi": true,
}

var exploreShellAllowedGitSubs = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true,
	"branch": true, "remote": true, "blame": true,
	"ls-files": true, "ls-tree": true, "rev-parse": true,
	"describe": true, "shortlog": true, "reflog": true,
	"tag":      true,
	"cat-file": true, "rev-list": true, "name-rev": true,
	"grep": true,
}

// exploreShellAllowedIvaldiSubs is the read-only allowlist for ivaldi
// subcommands in explore mode. Mirrors the intent of the git subs: status,
// history, diff, and inspection only — no gather/seal/timeline mutations.
var exploreShellAllowedIvaldiSubs = map[string]bool{
	"status": true, "log": true, "diff": true, "whereami": true,
	"whodidit": true,
	"help":     true,
	"timeline": true, // list only — sub-actions checked separately
}

var exploreShellAllowedGoSubs = map[string]bool{
	"version": true, "env": true, "list": true, "doc": true, "vet": true,
}

func IsExploreReadOnlyShell(command string) (bool, string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return false, "empty command"
	}
	if strings.ContainsRune(command, '`') {
		return false, "command substitution (backticks) is not allowed in explore mode"
	}
	if strings.Contains(command, "$(") {
		return false, "command substitution $(...) is not allowed in explore mode"
	}
	if HasOutputRedirect(command) {
		return false, "output redirection (>, >>) is not allowed in explore mode"
	}
	segments := SplitShellSegments(command)
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		fields := strings.Fields(seg)
		for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		bin := fields[0]
		if idx := strings.LastIndexAny(bin, "/"); idx >= 0 {
			bin = bin[idx+1:]
		}
		if !exploreShellAllowedBins[bin] {
			return false, fmt.Sprintf("command %q is not in the explore-mode read-only allowlist", bin)
		}
		switch bin {
		case "git":
			sub := FirstNonFlagArg(fields[1:])
			if sub != "" && !exploreShellAllowedGitSubs[sub] {
				return false, fmt.Sprintf("git subcommand %q is not in the explore-mode read-only allowlist", sub)
			}
		case "go":
			sub := FirstNonFlagArg(fields[1:])
			if sub != "" && !exploreShellAllowedGoSubs[sub] {
				return false, fmt.Sprintf("go subcommand %q is not in the explore-mode read-only allowlist", sub)
			}
		case "ivaldi":
			sub := FirstNonFlagArg(fields[1:])
			if sub != "" && !exploreShellAllowedIvaldiSubs[sub] {
				return false, fmt.Sprintf("ivaldi subcommand %q is not in the explore-mode read-only allowlist", sub)
			}
			// ivaldi timeline has sub-actions; only 'list' is read-only.
			if sub == "timeline" {
				subsub := FirstNonFlagArg(fields[2:])
				if subsub != "" && subsub != "list" {
					return false, fmt.Sprintf("ivaldi timeline %q is not read-only; only 'list' is allowed in explore mode", subsub)
				}
			}
		}
	}
	return true, ""
}

// InterceptVCSBypass checks a shell command for bare `git` invocations that
// would bypass the MCP translation layer. In an ivaldi repo, raw `git` via
// run_shell fails ("not a git repository") and wastes a round-trip; the
// git_* MCP tools are the correct path because they translate transparently.
//
// Returns ok=true if the command is safe to run, ok=false with a reason
// explaining what to do instead. The vcs backend is a parameter so the
// function is testable without touching the filesystem.
//
// Like IsExploreReadOnlyShell, this parses per-segment (split on |, &&, ;)
// and only inspects the leading binary — it cannot see into nested scripts
// or subprocesses that invoke git internally. That's an accepted limitation;
// the common failure mode is the model directly typing `git status`.
func InterceptVCSBypass(command, vcs string) (bool, string) {
	if vcs != "ivaldi" {
		return true, ""
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return true, ""
	}
	for _, seg := range SplitShellSegments(command) {
		if seg == "" {
			continue
		}
		fields := strings.Fields(seg)
		// Skip env-assignment prefixes (e.g. GIT_DIR=/foo git status).
		for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		bin := fields[0]
		if idx := strings.LastIndexAny(bin, "/"); idx >= 0 {
			bin = bin[idx+1:]
		}
		if bin == "git" {
			sub := FirstNonFlagArg(fields[1:])
			detail := ""
			if sub != "" {
				detail = fmt.Sprintf(" (subcommand %q)", sub)
			}
			return false, fmt.Sprintf(
				"this is an ivaldi repo, so `git` via run_shell will fail%s. "+
					"Use the git_%s MCP tool (it auto-translates to ivaldi), "+
					"or run `ivaldi` directly.",
				detail, GitToolNameForSub(sub),
			)
		}
	}
	return true, ""
}

// GitToolNameForSub maps a git subcommand to the corresponding MCP git_*
// tool name, for the bypass rejection message. Returns "status" as a
// sensible default when the subcommand is unknown or absent.
func GitToolNameForSub(sub string) string {
	switch sub {
	case "status":
		return "status"
	case "diff":
		return "diff"
	case "log":
		return "log"
	case "show":
		return "show"
	case "branch":
		return "branch"
	case "checkout":
		return "checkout"
	case "add":
		return "add"
	case "commit":
		return "commit"
	case "merge":
		return "merge"
	case "reset":
		return "reset"
	case "stash":
		return "stash"
	case "pull":
		return "pull"
	case "push":
		return "push"
	case "remote":
		return "remote"
	default:
		return "status"
	}
}

func FirstNonFlagArg(fields []string) string {
	for _, f := range fields {
		// A descriptor duplication such as 2>&1 is shell syntax, not a
		// positional argument (and therefore not a git/ivaldi subcommand).
		if !strings.HasPrefix(f, "-") && !IsFDDuplication(f) {
			return f
		}
	}
	return ""
}

func IsFDDuplication(s string) bool {
	for _, op := range []string{">&", "<&"} {
		if before, after, ok := strings.Cut(s, op); ok {
			left, right := before, after
			return (left == "" || AllASCIIDigits(left)) &&
				(right == "-" || AllASCIIDigits(right))
		}
	}
	return false
}

func AllASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func HasOutputRedirect(s string) bool {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '>' && !inSingle && !inDouble:
			// `>&` is fd duplication (e.g. 2>&1), not a file write.
			if i+1 < len(s) && s[i+1] == '&' {
				continue
			}
			return true
		}
	}
	return false
}

// SplitShellSegments breaks a command on |, ||, &&, and ; while leaving the
// contents of single- and double-quoted strings intact. This is a deliberate
// approximation — it doesn't handle every shell edge case, just enough to
// identify the leading binary of each pipeline segment.
func SplitShellSegments(command string) []string {
	var (
		segments []string
		cur      strings.Builder
		inSingle bool
		inDouble bool
	)
	flush := func() {
		segments = append(segments, strings.TrimSpace(cur.String()))
		cur.Reset()
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case c == '\\' && i+1 < len(command):
			cur.WriteByte(c)
			cur.WriteByte(command[i+1])
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			cur.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			cur.WriteByte(c)
		case !inSingle && !inDouble && c == '|':
			if i+1 < len(command) && command[i+1] == '|' {
				i++
			}
			flush()
		case !inSingle && !inDouble && c == '&':
			if i+1 < len(command) && command[i+1] == '&' {
				i++
				flush()
			} else {
				cur.WriteByte(c)
			}
		case !inSingle && !inDouble && c == ';':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		flush()
	}
	return segments
}

func ExtractShellCommand(raw json.RawMessage) string {
	var a struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(raw, &a)
	return a.Command
}

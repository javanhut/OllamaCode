// Package safeshell decides which shell commands a model may run in explore
// mode (read-only allowlist) and vets commands that try to bypass the git_*
// tools with bare `git` invocations in an ivaldi repo.
package safeshell

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
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

// IsExploreReadOnlyShell reports whether a command line may run in explore
// mode. The line is parsed into an AST (see Parse); anything the parser does
// not fully understand is rejected, and every simple command's argv is
// checked, not just its name.
func IsExploreReadOnlyShell(command string) (bool, string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return false, "empty command"
	}
	p := Parse(command)
	if p.Complex {
		return false, "not allowed in explore mode: " + p.Reason()
	}
	if len(p.Commands) == 0 {
		return false, "empty command"
	}
	for _, c := range p.Commands {
		if p.SetsVars && hasDyn(c) {
			return false, "a variable assigned earlier on the line is used, so the command cannot be checked"
		}
		if ok, reason := CommandReadOnly(c); !ok {
			return false, reason
		}
	}
	return true, ""
}

func hasDyn(c Command) bool {
	for _, d := range c.Dyn {
		if d {
			return true
		}
	}
	return false
}

// trustedBinDirs are the directories a program may be named from. A relative
// path (./ls) or an arbitrary one names a file the model may have written.
var trustedBinDirs = map[string]bool{
	"/bin": true, "/usr/bin": true, "/usr/local/bin": true, "/sbin": true,
	"/usr/sbin": true, "/opt/homebrew/bin": true,
}

// dangerousAssign reports environment assignments that change what a program
// runs or loads, so `PATH=/tmp/x git status` is not a read-only git status.
func dangerousAssign(name string) bool {
	switch name {
	case "PATH", "IFS", "ENV", "BASH_ENV", "SHELLOPTS", "BASHOPTS", "PS4", "PROMPT_COMMAND",
		"PAGER", "EDITOR", "VISUAL", "SHELL", "HOME", "TMPDIR", "CDPATH", "GOFLAGS", "GOTOOLCHAIN":
		return true
	}
	return strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "GIT_") ||
		strings.HasPrefix(name, "GO") && name != "GOOS" && name != "GOARCH"
}

// AssignsSafe reports whether none of the command's NAME=value prefixes change
// what runs or loads.
func AssignsSafe(c Command) bool {
	for _, a := range c.Assigns {
		if dangerousAssign(a.Name) {
			return false
		}
	}
	return true
}

// RedirectsBenign reports whether the command's redirections cannot write
// anywhere interesting: descriptor duplication, reads, here-documents, and
// writes to /dev/null.
func RedirectsBenign(c Command) bool {
	for _, r := range c.Redirects {
		if r.Write && (r.Target != "/dev/null" || r.Dyn) {
			return false
		}
	}
	return true
}

// CommandReadOnly reports whether one simple command is on the explore-mode
// read-only allowlist, checking its argv, assignments and redirections.
func CommandReadOnly(c Command) (bool, string) {
	for _, a := range c.Assigns {
		if dangerousAssign(a.Name) {
			return false, fmt.Sprintf("assigning %s changes what runs, so it is not allowed in explore mode", a.Name)
		}
	}
	for _, r := range c.Redirects {
		if r.Write {
			return false, "output redirection (>, >>) is not allowed in explore mode"
		}
	}
	if len(c.Argv) == 0 {
		return true, ""
	}
	if c.Inner {
		return false, "a command run through a wrapper is not allowed in explore mode"
	}
	first := c.Argv[0]
	if strings.Contains(first, "/") {
		dir, _ := path.Split(first)
		if !trustedBinDirs[strings.TrimSuffix(dir, "/")] {
			return false, fmt.Sprintf("command %q is run by path, which is not allowed in explore mode", first)
		}
	}
	bin := c.Bin()
	if !exploreShellAllowedBins[bin] {
		return false, fmt.Sprintf("command %q is not in the explore-mode read-only allowlist", bin)
	}
	args := c.Argv[1:]
	for i := range args {
		if c.Glob[i+1] && strings.HasPrefix(args[i], "-") {
			return false, "a glob that expands to a flag is not allowed in explore mode"
		}
	}
	if _, checked := exploreForbiddenFlags[bin]; checked || bin == "git" || bin == "go" || bin == "ivaldi" || bin == "command" {
		for i := range args {
			if c.Dyn[i+1] {
				return false, fmt.Sprintf("%s with a variable argument cannot be checked, so it is not allowed in explore mode", bin)
			}
		}
	}
	if ok, reason := exploreArgsReadOnly(bin, args); !ok {
		return false, reason
	}
	switch bin {
	case "git":
		sub, idx, ok, reason := GitSubcommand(args)
		if !ok {
			return false, reason
		}
		for i := 0; i <= idx && i < len(args); i++ {
			if c.Glob[i+1] {
				return false, "a glob in the git subcommand position is not allowed in explore mode"
			}
		}
		if sub != "" && !exploreShellAllowedGitSubs[sub] {
			return false, fmt.Sprintf("git subcommand %q is not in the explore-mode read-only allowlist", sub)
		}
		if sub != "" {
			if ok, reason := gitSubReadOnly(sub, args[idx+1:]); !ok {
				return false, reason
			}
		}
	case "go":
		sub := FirstNonFlagArg(args)
		if sub != "" && !exploreShellAllowedGoSubs[sub] {
			return false, fmt.Sprintf("go subcommand %q is not in the explore-mode read-only allowlist", sub)
		}
	case "ivaldi":
		sub := FirstNonFlagArg(args)
		if sub != "" && !exploreShellAllowedIvaldiSubs[sub] {
			return false, fmt.Sprintf("ivaldi subcommand %q is not in the explore-mode read-only allowlist", sub)
		}
		// ivaldi timeline has sub-actions; only 'list' is read-only.
		if sub == "timeline" {
			subsub := FirstNonFlagArg(args[1:])
			if subsub != "" && subsub != "list" {
				return false, fmt.Sprintf("ivaldi timeline %q is not read-only; only 'list' is allowed in explore mode", subsub)
			}
		}
	}
	return true, ""
}

// gitSafeGlobals are git options that may precede the subcommand without
// changing what runs. -C and friends take a value; the rest are flags.
var gitSafeGlobals = map[string]bool{
	"--no-pager": true, "-P": true, "-p": true, "--paginate": true, "--no-optional-locks": true,
	"--no-replace-objects": true, "--literal-pathspecs": true, "--glob-pathspecs": true,
	"--noglob-pathspecs": true, "--icase-pathspecs": true, "--no-advice": true,
	"--version": true, "--help": true, "-h": true,
}

// gitValueGlobals take their value as the next word (the `=` form is one word).
var gitValueGlobals = map[string]bool{
	"-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--config-env": true, "--super-prefix": true,
}

// GitSubcommand finds the real git subcommand in args (everything after
// `git`), skipping global options such as `-C dir` and `--no-pager`. idx is
// the subcommand's index in args. ok is false when a global option could
// change what runs (`-c k=v`, `--exec-path`, `--git-dir`, ...) or is unknown,
// because then the subcommand cannot be trusted.
func GitSubcommand(args []string) (sub string, idx int, ok bool, reason string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C":
			i++
			if i >= len(args) {
				return "", 0, false, "git -C needs a directory"
			}
		case gitSafeGlobals[a]:
		case strings.HasPrefix(a, "-"):
			return "", 0, false, fmt.Sprintf("git %s can change what git runs, so it is not allowed in explore mode", a)
		default:
			return a, i, true, ""
		}
	}
	return "", len(args), true, ""
}

// GitStripGlobals drops git's global options so `git -C d --no-pager push`
// reads as `git push`. With all=false only the safe ones are dropped, so a
// line using `-c` or `--exec-path` no longer looks like a plain subcommand;
// with all=true every global option goes, for deny rules to see the real
// subcommand.
func GitStripGlobals(argv []string, all bool) []string {
	if len(argv) == 0 || path.Base(argv[0]) != "git" {
		return argv
	}
	out := []string{argv[0]}
	i := 1
	for i < len(argv) {
		a := argv[i]
		switch {
		case a == "-C" || all && gitValueGlobals[a]:
			i += 2
		case gitSafeGlobals[a] || all && strings.HasPrefix(a, "-"):
			i++
		default:
			return append(out, argv[i:]...)
		}
	}
	return out
}

// MatchTexts lists the strings a rule pattern should be tried against for
// this command. The first is the command as written; for git it is followed
// by the form with global options removed. For deny/ask matching the program
// path is also reduced to its base name and git's global options are all
// dropped, so `/usr/bin/git -c x=y push` is seen as `git push`.
func (c Command) MatchTexts(deny bool) []string {
	argv := c.Argv
	if len(argv) == 0 {
		return nil
	}
	texts := []string{strings.Join(argv, " ")}
	add := func(a []string) {
		t := strings.Join(a, " ")
		if slices.Contains(texts, t) {
			return
		}
		texts = append(texts, t)
	}
	if deny {
		base := append([]string{path.Base(argv[0])}, argv[1:]...)
		add(base)
		add(GitStripGlobals(base, true))
	} else {
		add(GitStripGlobals(argv, false))
	}
	return texts
}

// exploreForbiddenFlags are arguments that turn an allowlisted reader into a
// writer or a command runner. A flag matches exactly, or as a prefix of an
// attached value (-ofile, --output=file).
var exploreForbiddenFlags = map[string][]string{
	"find":     {"-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls"},
	"fd":       {"-x", "--exec", "-X", "--exec-batch"},
	"rg":       {"--pre"},
	"sort":     {"-o", "--output"},
	"tree":     {"-o"},
	"date":     {"-s", "--set"},
	"file":     {"-C", "--compile"},
	"go":       {"-toolexec", "-vettool", "-exec", "-w", "-u"},
	"git":      {"--config-env", "--exec-path", "--output", "--ext-diff", "-O", "--open-files-in-pager", "--textconv"},
	"hostname": {"-F", "--file", "-b", "--boot"},
}

// exploreArgsReadOnly rejects arguments that make an allowlisted binary write
// or run something. The allowlist alone only sees the first word, so without
// this `find . -delete` or `env rm -rf x` pass as read-only.
func exploreArgsReadOnly(bin string, args []string) (bool, string) {
	for _, a := range args {
		if bin == "go" && strings.HasPrefix(a, "--") {
			a = a[1:] // the go tool accepts -flag and --flag alike
		}
		for _, f := range exploreForbiddenFlags[bin] {
			if a == f || strings.HasPrefix(a, f+"=") ||
				(len(f) == 2 && f[0] == '-' && f[1] != '-' && strings.HasPrefix(a, f) && len(a) > 2 && bin != "git" && bin != "go") {
				return false, fmt.Sprintf("%s %s can modify files or run other commands, so it is not allowed in explore mode", bin, f)
			}
		}
	}
	switch bin {
	case "env":
		// env with a command after its assignments runs that command.
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && !strings.Contains(a, "=") {
				return false, "env runs its remaining arguments as a command, so only bare `env` is allowed in explore mode"
			}
		}
	case "command":
		// `command -v x` looks x up; `command x` runs it.
		if len(args) == 0 || (args[0] != "-v" && args[0] != "-V") {
			return false, "only `command -v` / `command -V` are allowed in explore mode"
		}
	case "hostname":
		if FirstNonFlagArg(args) != "" {
			return false, "hostname with an argument sets the hostname, so it is not allowed in explore mode"
		}
	}
	return true, ""
}

// gitSubReadOnly narrows the allowlisted git subcommands that also have a
// writing form: branch/tag create and delete, remote add/remove, reflog
// expire/delete.
func gitSubReadOnly(sub string, rest []string) (bool, string) {
	positional := FirstNonFlagArg(rest)
	hasFlag := func(flags ...string) bool {
		for _, a := range rest {
			for _, f := range flags {
				if a == f || strings.HasPrefix(a, f+"=") {
					return true
				}
			}
		}
		return false
	}
	switch sub {
	case "branch":
		if hasFlag("-d", "-D", "--delete", "-m", "-M", "--move", "-c", "-C", "--copy", "-u", "--set-upstream-to", "--unset-upstream", "--edit-description", "-f", "--force") {
			return false, "git branch with that flag changes branches, so it is not allowed in explore mode"
		}
		if positional != "" && !hasFlag("-l", "--list", "--contains", "--no-contains", "--merged", "--no-merged", "--points-at") {
			return false, "git branch <name> creates a branch; use `git branch --list` in explore mode"
		}
	case "tag":
		if hasFlag("-d", "--delete", "-a", "--annotate", "-s", "--sign", "-f", "--force", "-m", "--message", "-F", "--file", "-u", "--local-user") {
			return false, "git tag with that flag changes tags, so it is not allowed in explore mode"
		}
		if positional != "" && !hasFlag("-l", "--list", "--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "-n") {
			return false, "git tag <name> creates a tag; use `git tag --list` in explore mode"
		}
	case "remote":
		switch positional {
		case "", "show", "get-url":
		default:
			return false, fmt.Sprintf("git remote %s changes repo config, so it is not allowed in explore mode", positional)
		}
	case "reflog":
		switch positional {
		case "", "show", "exists":
		default:
			if !strings.Contains(positional, "@") && positional != "HEAD" {
				return false, fmt.Sprintf("git reflog %s is not read-only, so it is not allowed in explore mode", positional)
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
// Like IsExploreReadOnlyShell, this parses the line into simple commands and
// inspects each one's program name — it cannot see into scripts or
// subprocesses that invoke git internally. That's an accepted limitation;
// the common failure mode is the model directly typing `git status`.
func InterceptVCSBypass(command, vcs string) (bool, string) {
	if vcs != "ivaldi" {
		return true, ""
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return true, ""
	}
	for _, c := range Parse(command).Commands {
		if c.Bin() == "git" {
			sub := FirstNonFlagArg(GitStripGlobals(c.Argv, true)[1:])
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

func ExtractShellCommand(raw json.RawMessage) string {
	var a struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(raw, &a)
	return a.Command
}

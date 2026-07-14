package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DetectVCS is the exported form of detectVCS, so callers outside this package
// (e.g. the TUI's environment block) can report the SAME backend the git_*
// tools actually run against.
func DetectVCS() string { return detectVCS() }

// detectVCS returns "ivaldi" if a .ivaldi directory exists in the current
// working directory (or a parent), otherwise "git". The git_* MCP tools use
// this to pick the right backend so they work transparently in either repo
// type.
func detectVCS() string {
	dir, err := os.Getwd()
	if err != nil {
		return "git"
	}
	for {
		if _, err := os.Stat(dir + "/.ivaldi"); err == nil {
			return "ivaldi"
		}
		parent := dir + "/.."
		if resolved, err := filepath.Abs(parent); err == nil {
			parent = resolved
		}
		if parent == dir {
			break
		}
		dir = parent
	}
	return "git"
}

// vcsExec builds an exec.Cmd for the detected VCS backend. toolName is the
// git_* tool name (e.g. "git_status"); gitArgv is the argument list the tool
// would pass to git (e.g. ["status", "-sb"]). For git backends this is a
// direct passthrough. For ivaldi the subcommands/flags are translated.
//
// If ivaldi has no equivalent for a tool, vcsExec returns a Cmd whose
// CombinedOutput will fail with a clear message — but callers that know
// they may be unsupported (e.g. stash) should check isVCSUnsupported first.
func vcsExec(ctx context.Context, toolName string, gitArgv ...string) *exec.Cmd {
	backend := detectVCS()
	if backend == "git" {
		return exec.CommandContext(ctx, "git", gitArgv...)
	}
	// ivaldi — translate.
	ivaldiArgv, ok := translateToIvaldi(toolName, gitArgv)
	if !ok {
		// Return a command that will produce a clear error message. We use
		// "false" so the exit code is non-zero and CombinedOutput captures
		// our message on stderr.
		msg := "ivaldi has no equivalent for " + toolName
		return exec.CommandContext(ctx, "sh", "-c", "echo '"+msg+"' >&2; exit 1")
	}
	return exec.CommandContext(ctx, "ivaldi", ivaldiArgv...)
}

// isVCSUnsupported reports whether the detected VCS backend lacks an
// equivalent for the named tool. Callers can use this to return a helpful
// message instead of attempting a doomed command.
func isVCSUnsupported(toolName string) bool {
	backend := detectVCS()
	if backend != "ivaldi" {
		return false
	}
	_, ok := translateToIvaldi(toolName, nil)
	return !ok
}

// translateToIvaldi converts a git argv list for the named tool into the
// equivalent ivaldi argv. Returns ok=false if there is no mapping.
//
// The mapping is based on ivaldi's command surface (verified via
// `ivaldi <sub> --help`):
//
//	git status     → ivaldi status
//	git diff       → ivaldi diff (--staged, positional refs; no --color)
//	git log        → ivaldi log --oneline --limit N (no author/since/grep/path filters)
//	git add        → ivaldi gather <paths>
//	git commit     → ivaldi seal -m <msg>   (--amend → ivaldi reseal -m <msg>)
//	git branch     → ivaldi timeline list/create/remove
//	git checkout   → ivaldi timeline switch <name> / timeline create <name>
//	git reset      → ivaldi discard <file> (unstage) / reverse --all (hard) / rewind <ref>
//	git merge      → ivaldi fuse <branch>
//	git stash      → NO EQUIVALENT
//	git pull       → ivaldi sync [timeline]
//	git push       → ivaldi upload [branch] [--force]
//	git remote     → ivaldi portal list/add/remove
func translateToIvaldi(toolName string, gitArgv []string) ([]string, bool) {
	// gitArgv[0] is the git subcommand (e.g. "diff", "branch"); the positional
	// translators below treat their args as data, so strip it here once.
	// nil-safe for isVCSUnsupported's probe call.
	if len(gitArgv) > 0 {
		gitArgv = gitArgv[1:]
	}
	switch toolName {
	case "git_status":
		return translateStatus(gitArgv), true

	case "git_diff":
		return translateDiff(gitArgv), true

	case "git_log":
		return translateLog(gitArgv), true

	case "git_add":
		return translateAdd(gitArgv), true

	case "git_commit":
		return translateCommit(gitArgv), true

	case "git_branch":
		return translateBranch(gitArgv), true

	case "git_checkout":
		return translateCheckout(gitArgv), true

	case "git_reset":
		return translateReset(gitArgv), true

	case "git_merge":
		return translateMerge(gitArgv), true

	case "git_stash":
		return nil, false

	case "git_pull":
		return translatePull(gitArgv), true

	case "git_push":
		return translatePush(gitArgv), true

	case "git_remote":
		return translateRemote(gitArgv), true
	}
	return nil, false
}

// translateStatus: git status → ivaldi status
// git flags (-sb, -s) are dropped; ivaldi status takes none of them.
func translateStatus(_ []string) []string {
	return []string{"status"}
}

// translateDiff: git diff → ivaldi diff
// git diff --staged → ivaldi diff --staged
// git diff <from> [<to>] [-- path] → ivaldi diff <from> [<to>] [path]
// ivaldi diff accepts positional TARGETS (timeline names, seal names, hash
// prefixes) and does NOT understand "--color=never" or "-- <path>" syntax.
// We pass refs and paths as positional arguments; flags git-only are dropped.
func translateDiff(gitArgv []string) []string {
	var out []string
	out = append(out, "diff")
	for _, arg := range gitArgv {
		switch {
		case arg == "--color=never" || arg == "--color":
			// drop — ivaldi doesn't colorize
		case arg == "--staged" || arg == "--cached":
			out = append(out, "--staged")
		case arg == "--":
			// ivaldi has no path separator; just skip it — the next args
			// become positional targets, which ivaldi accepts.
		case strings.Contains(arg, "..."):
			// git two-commit compare "A...B" → ivaldi takes the refs as
			// separate positional TARGETS, not git's range syntax.
			for _, p := range strings.SplitN(arg, "...", 2) {
				if p != "" {
					out = append(out, p)
				}
			}
		default:
			out = append(out, arg)
		}
	}
	return out
}

// translateLog: git log → ivaldi log --oneline --limit N
// ivaldi has no --graph, --author, --since, --grep, --date, --format, or
// path filter. We extract -n <count> and map it to --limit, defaulting to 10.
func translateLog(gitArgv []string) []string {
	out := []string{"log", "--oneline"}
	limit := "10"
	for i := 0; i < len(gitArgv); i++ {
		arg := gitArgv[i]
		switch {
		case arg == "-n" && i+1 < len(gitArgv):
			limit = gitArgv[i+1]
			i++
		case strings.HasPrefix(arg, "-n"):
			limit = arg[2:]
		case strings.HasPrefix(arg, "--author="),
			strings.HasPrefix(arg, "--since="),
			strings.HasPrefix(arg, "--grep="),
			strings.HasPrefix(arg, "--format="),
			strings.HasPrefix(arg, "--date="),
			arg == "--graph", arg == "--color=never",
			arg == "--", strings.HasPrefix(arg, "--"):
			// unsupported flags — drop
		default:
			// positional path or ref — ivaldi log doesn't support path
			// filtering, so we drop positionals too.
		}
	}
	out = append(out, "--limit", limit)
	return out
}

// translateAdd: git add <paths> → ivaldi gather <paths>
func translateAdd(gitArgv []string) []string {
	out := []string{"gather"}
	out = append(out, gitArgv...)
	return out
}

// translateCommit: git commit -m <msg> [--all] [--amend] → ivaldi seal -m <msg>
// git commit --amend → ivaldi reseal -m <msg>
// git commit --all → ivaldi gather . && ivaldi seal (handled by caller
// detecting --all and staging first; here we just translate the commit part)
func translateCommit(gitArgv []string) []string {
	var msg string
	amend := false
	for i := 0; i < len(gitArgv); i++ {
		arg := gitArgv[i]
		switch {
		case arg == "-m" && i+1 < len(gitArgv):
			msg = gitArgv[i+1]
			i++
		case strings.HasPrefix(arg, "-m"):
			msg = arg[2:]
		case arg == "--amend":
			amend = true
		case arg == "--all", arg == "-a":
			// handled by caller
		}
	}
	if amend {
		out := []string{"reseal"}
		if msg != "" {
			out = append(out, "-m", msg)
		}
		return out
	}
	out := []string{"seal"}
	if msg != "" {
		out = append(out, "-m", msg)
	}
	return out
}

// translateBranch: git branch [-r] [create <name>] [-d <name>] → ivaldi timeline ...
// git branch              → ivaldi timeline list
// git branch -r           → ivaldi timeline list (no remote-only listing)
// git branch <name>       → ivaldi timeline create <name>
// git branch -d <name>    → ivaldi timeline remove <name>
func translateBranch(gitArgv []string) []string {
	var name string
	create := false
	delete := false
	for _, arg := range gitArgv {
		switch arg {
		case "-d", "--delete":
			delete = true
		case "-r", "--remotes":
			// ivaldi has no remote-only listing; list all.
		default:
			if !strings.HasPrefix(arg, "-") && arg != "" {
				name = arg
			}
		}
	}
	switch {
	case delete:
		if name == "" {
			return []string{"timeline", "list"}
		}
		return []string{"timeline", "remove", name}
	case name != "":
		create = true
		_ = create
		return []string{"timeline", "create", name}
	default:
		return []string{"timeline", "list"}
	}
}

// translateCheckout: git checkout [-b] <target> → ivaldi timeline switch/create
// git checkout <branch>    → ivaldi timeline switch <branch>
// git checkout -b <branch> → ivaldi timeline create <branch> (creates AND switches)
// git checkout <file>      → unsupported by ivaldi in a general way — caller
// should detect this is a file and return an error.
func translateCheckout(gitArgv []string) []string {
	newBranch := false
	var target string
	for _, arg := range gitArgv {
		switch arg {
		case "-b":
			newBranch = true
		default:
			if !strings.HasPrefix(arg, "-") && arg != "" {
				target = arg
			}
		}
	}
	if target == "" {
		return []string{"timeline", "list"}
	}
	if newBranch {
		return []string{"timeline", "create", target}
	}
	return []string{"timeline", "switch", target}
}

// translateReset: git reset <target> [--mode] → ivaldi discard/reverse/rewind
// git reset <file>          → ivaldi discard <file> (unstage)
// git reset --hard <ref>    → ivaldi rewind <ref> --discard (moves head + rewrites tree)
// git reset --hard          → ivaldi reverse --all (discard working changes, no ref)
// git reset --soft <ref>    → ivaldi rewind <ref> (keeps working dir)
// git reset <ref> (mixed)  → ivaldi rewind <ref>
func translateReset(gitArgv []string) []string {
	var mode string
	var target string
	for _, arg := range gitArgv {
		switch arg {
		case "--hard":
			mode = "hard"
		case "--soft":
			mode = "soft"
		case "--mixed":
			mode = "mixed"
		case "HEAD", "--":
			// sentinel — skip
		default:
			if !strings.HasPrefix(arg, "-") && arg != "" {
				if target == "" {
					target = arg
				}
			}
		}
	}
	// If target looks like a file path (exists on disk), unstage it.
	if mode == "" {
		if target != "" {
			if info, err := os.Stat(target); err == nil && !info.IsDir() {
				return []string{"discard", target}
			}
			// mixed reset (default) to a ref — rewind head, keep working dir
			return []string{"rewind", target}
		}
		// git reset / git reset HEAD with no path = unstage everything.
		return []string{"discard"}
	}
	switch mode {
	case "hard":
		// With a ref, move head there and rewrite the tree; without one,
		// just throw away working changes against the last seal.
		if target != "" {
			return []string{"rewind", target, "--discard"}
		}
		return []string{"reverse", "--all"}
	case "soft":
		if target != "" {
			return []string{"rewind", target}
		}
		return []string{"status"} // soft reset to HEAD is a no-op
	case "mixed":
		if target != "" {
			return []string{"rewind", target}
		}
		return []string{"discard"} // unstage everything
	}
	return []string{"status"}
}

// translateMerge: git merge <branch> [--no-ff] → ivaldi fuse <branch>
// ivaldi fuse has no --no-ff equivalent; it uses --strategy.
func translateMerge(gitArgv []string) []string {
	var branch string
	for _, arg := range gitArgv {
		if !strings.HasPrefix(arg, "-") && arg != "" {
			if branch == "" {
				branch = arg
			}
		}
	}
	if branch == "" {
		return []string{"fuse"}
	}
	return []string{"fuse", branch}
}

// translatePull: git pull [--rebase] [<remote> [<branch>]] → ivaldi sync [timeline]
// ivaldi sync takes an optional timeline name, not remote/branch.
func translatePull(gitArgv []string) []string {
	var timeline string
	for _, arg := range gitArgv {
		switch arg {
		case "--rebase", "origin", "main":
			// drop git-specific
		default:
			if !strings.HasPrefix(arg, "-") && arg != "" {
				if timeline == "" {
					timeline = arg
				}
			}
		}
	}
	if timeline != "" {
		return []string{"sync", timeline}
	}
	return []string{"sync"}
}

// translatePush: git push [-u] [--force-with-lease] [<remote> [<branch>]] → ivaldi upload [branch] [--force]
// ivaldi upload takes an optional branch name; --force maps to --force.
func translatePush(gitArgv []string) []string {
	var branch string
	force := false
	for _, arg := range gitArgv {
		switch arg {
		case "-u", "--set-upstream":
			// ivaldi doesn't have upstream tracking the same way
		case "--force-with-lease", "--force", "-f":
			force = true
		case "origin", "main":
			// drop
		default:
			if !strings.HasPrefix(arg, "-") && arg != "" {
				if branch == "" {
					branch = arg
				}
			}
		}
	}
	out := []string{"upload"}
	if force {
		out = append(out, "--force")
	}
	if branch != "" {
		out = append(out, branch)
	}
	return out
}

// translateRemote: git remote [-v|add|remove|show] → ivaldi portal list|add|remove
// git remote            → ivaldi portal list
// git remote -v         → ivaldi portal list
// git remote add <n> <url> → ivaldi portal add <url> <name> (arg order may differ)
// git remote remove <n> → ivaldi portal remove <name>
// git remote show <n>   → ivaldi portal list (no show subcommand)
func translateRemote(gitArgv []string) []string {
	var action, name, url string
	for _, arg := range gitArgv {
		switch arg {
		case "-v":
			// verbose listing — same as list
		case "add", "remove", "show":
			action = arg
		default:
			if !strings.HasPrefix(arg, "-") && arg != "" {
				if name == "" {
					name = arg
				} else {
					url = arg
				}
			}
		}
	}
	if action == "" {
		action = "list"
	}
	switch action {
	case "add":
		// git remote add <name> <url>; ivaldi portals are keyed by owner/repo,
		// which we derive from the URL. Fall back to name if it's already
		// owner/repo (no URL given).
		repo := gitURLToOwnerRepo(url)
		if repo == "" {
			repo = gitURLToOwnerRepo(name)
		}
		return []string{"portal", "add", repo}
	case "remove":
		// ivaldi portal remove also takes owner/repo (what `portal list`
		// shows), not a git remote alias — normalize whatever was passed.
		return []string{"portal", "remove", gitURLToOwnerRepo(name)}
	default:
		return []string{"portal", "list"}
	}
}

// gitURLToOwnerRepo reduces a git remote URL to the "owner/repo" identifier
// ivaldi portals use. Handles https, scp-style git@host:owner/repo, and a bare
// owner/repo. Returns the trimmed input if no owner/repo pair can be found.
// ponytail: assumes GitHub/GitLab-style owner/repo; a self-hosted deep path
// would need portal's --url — add that only if such a host actually shows up.
func gitURLToOwnerRepo(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".git")
	s = strings.ReplaceAll(s, ":", "/") // fold scp-style host:owner/repo
	var segs []string
	for p := range strings.SplitSeq(s, "/") {
		if p != "" {
			segs = append(segs, p)
		}
	}
	if len(segs) >= 2 {
		return segs[len(segs)-2] + "/" + segs[len(segs)-1]
	}
	return s
}

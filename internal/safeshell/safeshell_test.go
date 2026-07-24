package safeshell

import "testing"

func TestIsExploreReadOnlyShell(t *testing.T) {
	allowed := []string{
		"ls -la",
		"cat README.md",
		"head -n 20 main.go",
		"grep -rn foo .",
		"rg --files",
		"find . -name '*.go'",
		"git status",
		"git log --oneline -n 5",
		"git diff HEAD~1",
		"go version",
		"go list ./...",
		"ls | wc -l",
		"cat file.txt | grep foo | sort | uniq",
		"ps aux 2>&1",
		"ivaldi --help 2>&1 || ivaldi help 2>&1 || ivaldi 2>&1",
		"ivaldi status 2>&1",
		"ls -la && pwd",
		"FOO=bar env",
		"cat 'has > in name.txt'",
	}
	for _, cmd := range allowed {
		ok, reason := IsExploreReadOnlyShell(cmd)
		if !ok {
			t.Errorf("expected %q to be allowed; rejected: %s", cmd, reason)
		}
	}

	blocked := []string{
		"rm -rf /tmp/foo",
		"mv a b",
		"echo hi > out.txt",
		"cat a >> b",
		"sed -i 's/a/b/' file",
		"sudo cat /etc/shadow",
		"git push",
		"git commit -m oops",
		"git checkout main",
		"go build ./...",
		"go run main.go",
		"$(rm -rf /)",
		"`whoami`",
		"ls; rm foo",
		"ls && rm foo",
		"npm install",
		"curl https://example.com",
	}
	for _, cmd := range blocked {
		ok, _ := IsExploreReadOnlyShell(cmd)
		if ok {
			t.Errorf("expected %q to be blocked, but it was allowed", cmd)
		}
	}
}

func TestFirstNonFlagArgSkipsFDDuplication(t *testing.T) {
	if got := FirstNonFlagArg([]string{"--help", "2>&1"}); got != "" {
		t.Fatalf("fd duplication treated as positional argument: %q", got)
	}
	if got := FirstNonFlagArg([]string{"-q", "status", "2>&1"}); got != "status" {
		t.Fatalf("expected status subcommand, got %q", got)
	}
}

func TestInterceptVCSBypass(t *testing.T) {
	cases := []struct {
		name    string
		command string
		vcs     string
		ok      bool
	}{
		{"git repo allows git", "git status", "git", true},
		{"ivaldi blocks git status", "git status", "ivaldi", false},
		{"ivaldi blocks git diff", "git diff --staged", "ivaldi", false},
		{"ivaldi blocks git log", "git log --oneline -n 5", "ivaldi", false},
		{"ivaldi blocks git with path", "/usr/bin/git status", "ivaldi", false},
		{"ivaldi blocks git with env prefix", "GIT_DIR=/foo git status", "ivaldi", false},
		{"ivaldi blocks git in pipeline", "git status | grep main", "ivaldi", false},
		{"ivaldi blocks git after semicolon", "echo hi; git status", "ivaldi", false},
		{"ivaldi blocks git after &&", "echo hi && git status", "ivaldi", false},
		{"ivaldi allows ivaldi directly", "ivaldi status", "ivaldi", true},
		{"ivaldi allows non-git commands", "ls -la", "ivaldi", true},
		{"ivaldi allows go test", "go test ./...", "ivaldi", true},
		{"empty command allowed", "", "ivaldi", true},
		{"git in single quotes not parsed", "echo 'git status'", "ivaldi", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := InterceptVCSBypass(c.command, c.vcs)
			if ok != c.ok {
				t.Errorf("InterceptVCSBypass(%q, %q) = %v, want %v (reason: %s)", c.command, c.vcs, ok, c.ok, reason)
			}
			if !ok && reason == "" {
				t.Errorf("expected a non-empty rejection reason for %q", c.command)
			}
		})
	}
}

// TestIsExploreReadOnlyShellAdversarial documents how the parser handles
// bypass attempts. Every expectation here is CURRENT behavior — cases marked
// hole=true are ones where a mutating command is currently judged read-only;
// they are documented, not fixed, because this change is a pure move.
func TestIsExploreReadOnlyShellAdversarial(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool // true = parser allows the command in explore mode
		hole string
	}{
		// Output redirects.
		{"plain redirect", "echo hi > out.txt", false, ""},
		{"append redirect", "cat a >> b", false, ""},
		{"stderr redirect to file", "ls 2> err.txt", false, ""},
		{"redirect to /dev/null still blocked", "echo hi > /dev/null", false, ""},
		{"fd duplication is not a file write", "ls 1>&2", true, ""},
		{"redirect inside quotes ignored", "cat 'has > in name.txt'", true, ""},

		// Command substitution.
		{"dollar-paren substitution", "echo $(rm -rf /)", false, ""},
		{"backtick substitution", "echo `rm -rf /`", false, ""},
		{"backticks inside quotes are a false positive", "echo 'a ` b'", false, "false positive: the backtick check scans the raw command, so it also rejects benign quoted text"},
		{"dollar-paren inside quotes is a false positive", "echo 'cost is $(x)'", false, "false positive: the $( check scans the raw command, so it also rejects benign quoted text"},

		// Chaining.
		{"semicolon chains a mutating command", "ls; rm foo", false, ""},
		{"&& chains a mutating command", "ls && rm foo", false, ""},
		{"|| chains a mutating command", "ls || rm foo", false, ""},
		{"pipe into a mutating command", "ls | rm foo", false, ""},
		{"pipe into allowlisted chain", "cat file | grep foo | sort", true, ""},

		// Allowlisted-binary abuse.
		{"git commit blocked by subcommand allowlist", "git commit -m oops", false, ""},
		{"git push blocked by subcommand allowlist", "git push", false, ""},
		{"git tag is allowlisted but can mutate", "git tag v2.0", true, "HOLE: `git tag v2.0` creates a tag; `tag` is in the read-only subcommand allowlist and flags are not inspected"},
		{"git tag -d deletes a tag", "git tag -d v1.0", true, "HOLE: `git tag -d v1.0` deletes a tag; `tag` is allowlisted and the -d flag is not inspected"},
		{"git branch -D deletes a branch", "git branch -D feature", true, "HOLE: `git branch -D feature` force-deletes a branch; `branch` is allowlisted and the -D flag is not inspected"},
		{"git branch creates a branch", "git branch new-feature", true, "HOLE: `git branch new-feature` creates a branch; `branch` is allowlisted"},
		{"git remote can mutate", "git remote remove origin", true, "HOLE: `git remote remove origin` mutates repo config; `remote` is allowlisted and its sub-actions are not inspected"},
		{"find -exec runs arbitrary commands", "find . -exec rm {} +", true, "HOLE: `find` is allowlisted and its arguments are not inspected, so -exec/-ok execute arbitrary commands"},
		{"find -delete removes files", "find . -name '*.tmp' -delete", true, "HOLE: `find -delete` deletes files; find's arguments are not inspected"},
		{"xargs itself is not allowlisted", "xargs rm", false, ""},
		{"pipe into xargs rm", "ls | xargs rm", false, ""},
		{"sed -i blocked (sed not allowlisted)", "sed -i 's/a/b/' file", false, ""},
		{"awk system() blocked (awk not allowlisted)", "awk 'BEGIN{system(\"rm x\")}'", false, ""},
		{"env executes its arguments", "env rm -rf /tmp/foo", true, "HOLE: `env` is allowlisted and runs its remaining arguments as a command, so `env rm ...` bypasses the per-segment bin check"},
		{"command builtin executes its arguments", "command rm foo", true, "HOLE: `command` is allowlisted and runs its remaining arguments as a command"},

		// Env-prefixed commands.
		{"env assignment prefix skipped", "FOO=bar ls", true, ""},
		{"env assignment does not launder a bad bin", "FOO=bar rm x", false, ""},

		// Path tricks.
		{"absolute path to rm blocked by basename", "/bin/rm x", false, ""},
		{"relative path resolves to basename", "./ls", true, ""},
		{"absolute path to allowlisted bin", "/bin/ls -la", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := IsExploreReadOnlyShell(c.cmd)
			if ok != c.want {
				t.Errorf("IsExploreReadOnlyShell(%q) = %v, want %v (reason: %s)", c.cmd, ok, c.want, reason)
			}
			if c.hole != "" {
				t.Logf("%s", c.hole)
			}
		})
	}
}

// TestInterceptVCSBypassAdversarial documents how the bypass guard handles
// attempts to sneak `git` past it in an ivaldi repo. Expectations are CURRENT
// behavior; cases marked hole=true are bypasses the guard currently misses.
func TestInterceptVCSBypassAdversarial(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool // true = guard allows the command
		hole string
	}{
		{"direct git blocked", "git status", false, ""},
		{"git via absolute path blocked", "/usr/bin/git status", false, ""},
		{"git behind env assignment blocked", "GIT_DIR=/foo git status", false, ""},
		{"git later in a pipeline blocked", "cat x | git log", false, ""},
		{"git after semicolon blocked", "echo hi; git status", false, ""},
		{"git after || blocked", "false || git status", false, ""},
		{"git via env wrapper misses", "env git status", true, "HOLE: `env git status` bypasses the guard — only the leading binary is inspected, and env executes git"},
		{"git via command builtin misses", "command git status", true, "HOLE: `command git status` bypasses the guard — only the leading binary is inspected"},
		{"git via xargs misses", "xargs git", true, "HOLE: `xargs git` bypasses the guard — only the leading binary is inspected"},
		{"git via sh -c misses", "sh -c 'git status'", true, "HOLE: `sh -c 'git status'` bypasses the guard — nested scripts are invisible to the parser (documented accepted limitation)"},
		{"git via command substitution misses", "echo $(git status)", true, "HOLE: `$(git status)` bypasses the guard — command substitution is not split into segments (IsExploreReadOnlyShell blocks this in explore mode, but write/auto modes have no such check)"},
		{"git via backticks misses", "echo `git status`", true, "HOLE: backtick substitution bypasses the guard for the same reason"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := InterceptVCSBypass(c.cmd, "ivaldi")
			if ok != c.want {
				t.Errorf("InterceptVCSBypass(%q, \"ivaldi\") = %v, want %v (reason: %s)", c.cmd, ok, c.want, reason)
			}
			if c.hole != "" {
				t.Logf("%s", c.hole)
			}
		})
	}
}

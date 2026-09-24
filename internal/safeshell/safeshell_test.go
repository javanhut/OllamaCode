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
// bypass attempts. A non-empty hole marks a known limitation that is still
// accepted.
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
		{"git tag is allowlisted but can mutate", "git tag v2.0", false, ""},
		{"git tag -d deletes a tag", "git tag -d v1.0", false, ""},
		{"git branch -D deletes a branch", "git branch -D feature", false, ""},
		{"git branch creates a branch", "git branch new-feature", false, ""},
		{"git remote can mutate", "git remote remove origin", false, ""},
		{"find -exec runs arbitrary commands", "find . -exec rm {} +", false, ""},
		{"find -delete removes files", "find . -name '*.tmp' -delete", false, ""},
		{"xargs itself is not allowlisted", "xargs rm", false, ""},
		{"pipe into xargs rm", "ls | xargs rm", false, ""},
		{"sed -i blocked (sed not allowlisted)", "sed -i 's/a/b/' file", false, ""},
		{"awk system() blocked (awk not allowlisted)", "awk 'BEGIN{system(\"rm x\")}'", false, ""},
		{"env executes its arguments", "env rm -rf /tmp/foo", false, ""},
		{"command builtin executes its arguments", "command rm foo", false, ""},

		// Env-prefixed commands.
		{"env assignment prefix skipped", "FOO=bar ls", true, ""},
		{"env assignment does not launder a bad bin", "FOO=bar rm x", false, ""},

		// Path tricks.
		{"absolute path to rm blocked by basename", "/bin/rm x", false, ""},
		{"relative path resolves to basename", "./ls", true, ""},
		{"absolute path to allowlisted bin", "/bin/ls -la", true, ""},

		// Chaining the old splitter missed.
		{"newline chains a mutating command", "ls\nrm foo", false, ""},
		{"lone & backgrounds then chains", "ls & rm foo", false, ""},
		{"fd duplication still not a separator", "ls 2>&1 | grep x", true, ""},
		{"process substitution runs a command", "cat <(rm foo)", false, ""},

		// Argument-level escapes from allowlisted readers.
		{"find -execdir", "find . -execdir rm {} ;", false, ""},
		{"find -fprint writes a file", "find . -fprint out.txt", false, ""},
		{"plain find still allowed", "find . -name '*.go' -type f", true, ""},
		{"fd --exec", "fd . --exec rm", false, ""},
		{"rg --pre runs a preprocessor", "rg --pre ./evil foo", false, ""},
		{"rg --pre= form", "rg --pre=./evil foo", false, ""},
		{"plain rg allowed", "rg -n foo src", true, ""},
		{"sort -o writes a file", "sort -o out.txt in.txt", false, ""},
		{"sort -o attached", "sort -oout.txt in.txt", false, ""},
		{"sort --output=", "sort --output=out.txt in.txt", false, ""},
		{"plain sort allowed", "sort -u in.txt", true, ""},
		{"tree -o writes a file", "tree -o out.txt", false, ""},
		{"env with only assignments prints", "env FOO=bar", true, ""},
		{"bare env allowed", "env", true, ""},
		{"env -i then command", "env -i FOO=1 rm x", false, ""},
		{"command -v allowed", "command -v go", true, ""},
		{"hostname set", "hostname evil", false, ""},
		{"git diff --output writes", "git diff --output=patch.txt", false, ""},
		{"git -c config injection", "git -c core.pager=./evil log", false, ""},
		{"git --config-env", "git --config-env=core.pager=X log", false, ""},
		{"git log -c combined diff allowed", "git log -c -1", true, ""},
		{"git grep -O runs a pager", "git grep -O foo", false, ""},
		{"git diff --ext-diff", "git diff --ext-diff", false, ""},
		{"git branch list allowed", "git branch -a", true, ""},
		{"git branch --list pattern allowed", "git branch --list 'feat*'", true, ""},
		{"git branch --contains allowed", "git branch --contains HEAD", true, ""},
		{"git tag list allowed", "git tag", true, ""},
		{"git tag -l pattern allowed", "git tag -l 'v1*'", true, ""},
		{"git remote -v allowed", "git remote -v", true, ""},
		{"git remote add blocked", "git remote add x url", false, ""},
		{"git remote get-url allowed", "git remote get-url origin", true, ""},
		{"git reflog allowed", "git reflog -5", true, ""},
		{"git reflog expire blocked", "git reflog expire --all", false, ""},
		{"go vet -vettool runs a binary", "go vet -vettool=./evil ./...", false, ""},
		{"go env -w writes config", "go env -w GOFLAGS=x", false, ""},
		{"plain go vet allowed", "go vet ./...", true, ""},
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
		{"git via env wrapper misses", "env git status", true, "HOLE: the guard only inspects the leading binary, and `env` hides git behind it"},
		{"git via command builtin misses", "command git status", true, "HOLE: the guard only inspects the leading binary, and `command` hides git behind it"},
		{"git via xargs misses", "xargs git", true, "HOLE: git runs as an xargs argument, not a leading binary"},
		{"git via sh -c misses", "sh -c 'git status'", true, "HOLE: git runs inside a nested shell string"},
		{"git via command substitution misses", "echo $(git status)", true, "HOLE: git runs inside a command substitution"},
		{"git via backticks misses", "echo `git status`", true, "HOLE: git runs inside backticks"},
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

func TestIsVerifyReadOnlyShell(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"go test ./...", true},
		{"go test -run TestX -count=1 -cover ./tui", true},
		{"go test -v ./tui 2>&1 | tail -20", true},
		{"go vet ./... && go test ./tools", true},
		{"go test -coverprofile=c.out ./...", false},
		{"go test -args -test.coverprofile=c.out", false},
		{"go test -c -o bin ./tui", false},
		{"go test -fuzz=FuzzX ./tui", false},
		{"go test -exec rm ./tui", false},
		{"go test -mod=mod ./...", false},
		{"go build ./...", false},
		{"go generate ./...", false},
	}
	for _, c := range cases {
		if got, reason := IsVerifyReadOnlyShell(c.cmd); got != c.want {
			t.Errorf("%q: got %v (%s), want %v", c.cmd, got, reason, c.want)
		}
	}
	if ok, _ := IsExploreReadOnlyShell("go test ./..."); ok {
		t.Error("go test must stay out of explore")
	}
}

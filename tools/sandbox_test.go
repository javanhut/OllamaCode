package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSeatbeltProfileShape(t *testing.T) {
	p := seatbeltProfile([]string{"/ws", "/private/tmp"}, []string{"/ws/.git/hooks"}, []string{"/ws"}, false)

	if !strings.HasPrefix(p, "(version 1)\n") {
		t.Errorf("profile must open with a version declaration:\n%s", p)
	}
	// Default-allow keeps reads/process/network working; the only restriction
	// is file writes, and the deny must precede the per-root allows (seatbelt
	// applies the last matching rule).
	deny := strings.Index(p, "(deny file-write*)")
	allow := strings.Index(p, `(allow file-write* (subpath "/ws"))`)
	if deny < 0 || allow < 0 || deny > allow {
		t.Errorf("expected (deny file-write*) before the writable-root allows:\n%s", p)
	}
	for _, want := range []string{
		"(allow default)",
		`(allow file-write* (subpath "/private/tmp"))`,
		`(allow file-write* (literal "/dev/null"))`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("profile missing %q:\n%s", want, p)
		}
	}
}

func TestSeatbeltProfileEscapesQuotes(t *testing.T) {
	p := seatbeltProfile([]string{`/we"ird\path`}, nil, nil, true)
	if !strings.Contains(p, `(subpath "/we\"ird\\path")`) {
		t.Errorf("SBPL string escaping broken:\n%s", p)
	}
}

func TestBwrapArgvShape(t *testing.T) {
	argv := bwrapArgv("echo hi", []string{"/ws", "/tmp"}, []string{"/ws/.git/hooks"}, []string{"/var/run/docker.sock"}, false)
	joined := strings.Join(argv, " ")

	for _, want := range []string{"--ro-bind / /", "--dev /dev", "--proc /proc", "--bind /ws /ws", "--bind /tmp /tmp",
		"--unshare-net", "--ro-bind /ws/.git/hooks /ws/.git/hooks", "--ro-bind /dev/null /var/run/docker.sock"} {
		if !strings.Contains(joined, want) {
			t.Errorf("bwrap argv missing %q: %s", want, joined)
		}
	}
	// Protected re-binds must come after the writable binds: later mounts win.
	if strings.Index(joined, "--ro-bind /ws/.git/hooks") < strings.Index(joined, "--bind /ws /ws") {
		t.Errorf("protected ro-bind must follow the writable bind: %s", joined)
	}
	tail := argv[len(argv)-4:]
	if strings.Join(tail, " ") != "-- sh -c echo hi" {
		t.Errorf("argv must end with -- sh -c <cmd>, got %v", tail)
	}
	if open := strings.Join(bwrapArgv("true", nil, nil, nil, true), " "); strings.Contains(open, "--unshare-net") {
		t.Errorf("shell_network=true must keep the network shared: %s", open)
	}
}

func TestSeatbeltProfileNetwork(t *testing.T) {
	denied := seatbeltProfile([]string{"/ws"}, nil, nil, false)
	for _, want := range []string{
		"(deny network*)",
		`(allow network-outbound (remote ip "localhost:*"))`,
		`(allow network-bind (local ip "localhost:*"))`,
		`(deny mach-lookup (global-name "com.apple.dnssd.service"))`,
	} {
		if !strings.Contains(denied, want) {
			t.Errorf("network-denied profile missing %q:\n%s", want, denied)
		}
	}
	// The broad local-ip form matches outbound sockets and re-opens egress.
	if strings.Contains(denied, `(allow network* (local ip`) {
		t.Errorf("profile must not use the broad network* local-ip allow:\n%s", denied)
	}
	allowed := seatbeltProfile([]string{"/ws"}, nil, nil, true)
	if strings.Contains(allowed, "(deny network*)") {
		t.Errorf("shell_network=true must not deny network:\n%s", allowed)
	}
	if !strings.Contains(allowed, "docker.sock") {
		t.Errorf("docker socket must stay denied even with network allowed:\n%s", allowed)
	}
}

func TestSeatbeltProtectedDenyFollowsAllow(t *testing.T) {
	p := seatbeltProfile([]string{"/ws"}, []string{"/ws/.git/hooks"}, nil, true)
	allow := strings.Index(p, `(allow file-write* (subpath "/ws"))`)
	deny := strings.Index(p, `(deny file-write* (subpath "/ws/.git/hooks"))`)
	if allow < 0 || deny < 0 || deny < allow {
		t.Errorf("protected deny must come after the root allow (last match wins):\n%s", p)
	}
}

func TestSandboxProtectedPaths(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)
	resolved, _ := filepath.EvalSymlinks(root)
	hooks := filepath.Join(resolved, ".git", "hooks")

	if slices.Contains(sandboxProtectedPaths(false), hooks) {
		t.Errorf("git paths must not be protected before they exist (git init must work)")
	}
	if !slices.Contains(sandboxProtectedPaths(false), filepath.Join(resolved, ".vscode")) {
		t.Errorf("editor config must be protected even before it exists")
	}
	if slices.Contains(sandboxProtectedPaths(true), filepath.Join(resolved, ".vscode")) {
		t.Errorf("existingOnly must drop paths that are not on disk (bwrap cannot bind them)")
	}
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sandboxProtectedPaths(true), hooks) {
		t.Errorf("existing .git/hooks must be protected")
	}
	for path, want := range map[string]bool{
		filepath.Join(resolved, "AGENTS.md"):                 true,
		filepath.Join(resolved, "agents.md"):                 true,
		filepath.Join(resolved, ".claude", "settings.json"):  true,
		filepath.Join(resolved, ".git", "hooks", "pre-push"): true,
		filepath.Join(resolved, ".git", "objects", "ab"):     false,
		filepath.Join(resolved, "main.go"):                   false,
		filepath.Join(resolved, "docs", "AGENTS.md.bak"):     false,
	} {
		if got := IsProtectedPath(path); got != want {
			t.Errorf("IsProtectedPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestSandboxBlocksNetworkAndProtectedWrites runs the real wrapper: external
// egress and DNS fail, loopback works, and a protected path refuses writes.
func TestSandboxBlocksNetworkAndProtectedWrites(t *testing.T) {
	if detectShellSandbox() == "" {
		t.Skip("no OS sandbox on PATH")
	}
	root := t.TempDir()
	pinJail(t, root)
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	SetShellNetworkAllowed(false)

	run := func(command string) (string, error) {
		cmd := newShellCommand(command)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("echo x > .git/hooks/pre-commit"); err == nil {
		t.Errorf("write to .git/hooks must fail inside the sandbox, got %q", out)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if detectShellSandbox() == "sandbox-exec" {
		if out, err := run("echo x > pkg/AGENTS.md"); err == nil {
			t.Errorf("write to a nested AGENTS.md must fail inside the sandbox, got %q", out)
		}
	}
	if out, err := run("echo x > ok.txt"); err != nil {
		t.Errorf("ordinary workspace write must succeed: %v %s", err, out)
	}
	if _, err := exec.LookPath("python3"); err == nil {
		out, err := run(`python3 -c "import socket; socket.getaddrinfo('example.com', 443)"`)
		if err == nil {
			t.Errorf("DNS lookup must fail with network denied, got %q", out)
		}
		out, err = run(`python3 -c "
import socket
s = socket.socket(); s.bind(('127.0.0.1', 0)); s.listen(1)
c = socket.create_connection(s.getsockname(), timeout=3); c.close()"`)
		if err != nil {
			t.Errorf("loopback must stay open: %v %s", err, out)
		}
	}
}

func TestSandboxWritableDirs(t *testing.T) {
	root := t.TempDir()
	pinJail(t, root)

	dirs := sandboxWritableDirs()
	has := func(d string) bool {
		return slices.Contains(dirs, d)
	}
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	if !has(resolvedRoot) {
		t.Errorf("writable dirs missing the workspace root %q: %v", resolvedRoot, dirs)
	}
	if !has("/tmp") {
		t.Errorf("writable dirs missing /tmp: %v", dirs)
	}
	home, _ := os.UserHomeDir()
	if !has(filepath.Join(home, ".cache")) {
		t.Errorf("writable dirs missing ~/.cache (build caches): %v", dirs)
	}
}

// TestSandboxWritableDirsToolCaches pins the package-manager cache handling:
// env-relocated caches win, documented fallbacks apply, and dirs that do not
// exist are never added (bwrap bind-mounts must exist, and a package manager
// can extend but not create its cache under the sandbox).
func TestSandboxWritableDirsToolCaches(t *testing.T) {
	pinJail(t, t.TempDir())
	// Hermetic HOME: without it a real ~/.rustup or ~/go/pkg/mod on the dev
	// machine would leak into the assertions below.
	home := t.TempDir()
	t.Setenv("HOME", home)

	has := func(dirs []string, d string) bool {
		return slices.Contains(dirs, d)
	}

	t.Run("env overrides", func(t *testing.T) {
		gomod, cargo, npm := t.TempDir(), t.TempDir(), t.TempDir()
		t.Setenv("GOMODCACHE", gomod)
		t.Setenv("CARGO_HOME", cargo)
		t.Setenv("NPM_CONFIG_CACHE", npm)
		dirs := sandboxWritableDirs()
		for _, d := range []string{gomod, cargo, npm} {
			if !has(dirs, d) {
				t.Errorf("writable dirs missing env-relocated cache %q", d)
			}
		}
	})

	t.Run("gopath fallback", func(t *testing.T) {
		// No GOMODCACHE: the module cache is $GOPATH/pkg/mod (first entry).
		t.Setenv("GOMODCACHE", "")
		gopath := t.TempDir()
		modcache := filepath.Join(gopath, "pkg", "mod")
		if err := os.MkdirAll(modcache, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOPATH", gopath+string(os.PathListSeparator)+filepath.Join(gopath, "second"))
		if dirs := sandboxWritableDirs(); !has(dirs, modcache) {
			t.Errorf("writable dirs missing GOPATH-derived module cache %q: %v", modcache, dirs)
		}
	})

	t.Run("home defaults", func(t *testing.T) {
		// No env relocation at all: the per-user defaults under HOME apply.
		for _, env := range []string{"GOMODCACHE", "GOPATH", "CARGO_HOME", "NPM_CONFIG_CACHE"} {
			t.Setenv(env, "")
		}
		defaults := []string{
			filepath.Join(home, "go", "pkg", "mod"),
			filepath.Join(home, ".cargo"),
			filepath.Join(home, ".rustup"),
			filepath.Join(home, ".npm"),
		}
		for _, d := range defaults {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		dirs := sandboxWritableDirs()
		for _, d := range defaults {
			if !has(dirs, d) {
				t.Errorf("writable dirs missing default cache %q", d)
			}
		}
	})

	t.Run("nonexistent skipped", func(t *testing.T) {
		// A relocated-but-missing cache must not be added: under the sandbox
		// the tool could not create it anyway, and bwrap would fail to launch.
		// Fresh HOME: earlier subtests created the default caches under the
		// shared one.
		home := t.TempDir()
		t.Setenv("HOME", home)
		missing := filepath.Join(home, "no-such-dir")
		t.Setenv("GOMODCACHE", missing)
		t.Setenv("CARGO_HOME", missing)
		t.Setenv("NPM_CONFIG_CACHE", missing)
		dirs := sandboxWritableDirs()
		if has(dirs, missing) {
			t.Errorf("nonexistent cache dir %q must not be writable", missing)
		}
		for _, d := range []string{filepath.Join(home, ".rustup"), filepath.Join(home, ".npm")} {
			if has(dirs, d) {
				t.Errorf("nonexistent default cache %q must not be writable", d)
			}
		}
	})
}

func TestNewShellCommandWrapsWhenSandboxed(t *testing.T) {
	switch detectShellSandbox() {
	case "sandbox-exec":
		cmd := newShellCommand("true")
		if filepath.Base(cmd.Path) != "sandbox-exec" {
			t.Fatalf("expected sandbox-exec wrapper, got %v", cmd.Args)
		}
		joined := strings.Join(cmd.Args, " ")
		if !strings.Contains(joined, "-p ") || !strings.HasSuffix(joined, "sh -c true") {
			t.Fatalf("sandbox-exec argv malformed: %v", cmd.Args)
		}
	case "bwrap":
		cmd := newShellCommand("true")
		if filepath.Base(cmd.Path) != "bwrap" {
			t.Fatalf("expected bwrap wrapper, got %v", cmd.Args)
		}
	default:
		// No sandbox on PATH (e.g. minimal CI): the command must run unwrapped
		// exactly as before.
		cmd := newShellCommand("true")
		if filepath.Base(cmd.Path) != "sh" {
			t.Fatalf("expected plain sh without a sandbox, got %v", cmd.Args)
		}
	}
}

func TestNewShellCommandKillSwitch(t *testing.T) {
	SetShellSandboxEnabled(false)
	defer SetShellSandboxEnabled(true)
	cmd := newShellCommand("true")
	if filepath.Base(cmd.Path) != "sh" {
		t.Fatalf("shell_sandbox=false must run plain sh, got %v", cmd.Args)
	}
}

func TestSandboxNoticeOneTime(t *testing.T) {
	// Restore a pristine probe/notice state afterwards: re-probe the sandbox
	// and re-arm the one-time warning for whatever runs next.
	defer func() {
		sandboxKind.Store(nil)
		sandboxNoticeGiven.Store(false)
		SetShellSandboxEnabled(true)
	}()

	SetShellSandboxEnabled(true)
	sandboxNoticeGiven.Store(false)
	sandboxKind.Store(new(string)) // probed, nothing found
	first := withSandboxNotice("output")
	second := withSandboxNotice("output")
	if !strings.HasPrefix(first, sandboxMissingNotice) {
		t.Error("first unwrapped run should carry the warning")
	}
	if strings.HasPrefix(second, sandboxMissingNotice) {
		t.Error("warning must be one-time per process")
	}

	sandboxNoticeGiven.Store(false)
	kind := "bwrap" // a sandbox is available: never warn
	sandboxKind.Store(&kind)
	if got := withSandboxNotice("output"); got != "output" {
		t.Error("no warning expected when a sandbox wraps the command")
	}

	sandboxNoticeGiven.Store(false)
	sandboxKind.Store(new(string))
	SetShellSandboxEnabled(false) // explicit opt-out: stay silent
	if got := withSandboxNotice("output"); got != "output" {
		t.Error("no warning expected when the sandbox is disabled by config")
	}
}

// TestRunShellSandboxConfinement is the end-to-end proof: inside the sandbox,
// writes under the workspace succeed and writes outside it fail at the OS
// level. Skips when no sandbox binary exists (CI may be Linux without bwrap).
func TestRunShellSandboxConfinement(t *testing.T) {
	if detectShellSandbox() == "" {
		t.Skip("no OS sandbox (sandbox-exec/bwrap) on PATH")
	}
	SetShellSandboxEnabled(true)
	root := t.TempDir()
	t.Chdir(root)
	ctx := context.Background()
	shell := RunShellTool()
	// A nonzero exit comes back as *CommandFailure carrying the output, so the
	// helper reports the command's own output either way.
	run := func(args map[string]string) string {
		t.Helper()
		out, err := shell.Handler(ctx, jailArgs(t, args))
		if failed, ok := errors.AsType[*CommandFailure](err); ok {
			return failed.Output
		}
		if err != nil {
			t.Fatalf("run_shell %v: %v", args, err)
		}
		return out
	}

	// Writes under the workspace root succeed.
	run(map[string]string{"command": "echo hi > inroot.txt"})
	if data, err := os.ReadFile(filepath.Join(root, "inroot.txt")); err != nil || strings.TrimSpace(string(data)) != "hi" {
		t.Fatalf("in-root write failed: data=%q err=%v", data, err)
	}

	// stdin still flows through the wrapper.
	if out := run(map[string]string{"command": "cat", "stdin": "hello-stdin"}); !strings.Contains(out, "hello-stdin") {
		t.Fatalf("stdin broken under sandbox: %q", out)
	}

	// A write outside every writable root is denied by the OS. HOME itself is
	// not writable (only its cache dirs are), so the redirect fails and the
	// file must not exist afterwards.
	probe := filepath.Join(os.Getenv("HOME"), fmt.Sprintf("ocode-sandbox-probe-%d", os.Getpid()))
	out := run(map[string]string{"command": "echo hi > " + probe})
	if _, err := os.Stat(probe); !os.IsNotExist(err) {
		os.Remove(probe)
		t.Fatalf("sandbox allowed a write to %q", probe)
	}
	if !strings.Contains(out, "[exit ") {
		t.Fatalf("expected a non-zero exit for the denied write, got %q", out)
	}
}

// TestRunShellUnwrappedWhenAbsent documents the fallback: with no sandbox
// binary the command runs as plain sh -c and still works.
func TestRunShellUnwrappedWhenAbsent(t *testing.T) {
	if detectShellSandbox() != "" {
		t.Skip("sandbox present on this machine; fallback covered by TestNewShellCommandWrapsWhenSandboxed")
	}
	out, err := RunShellTool().Handler(context.Background(), jailArgs(t, map[string]string{"command": "echo ok"}))
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("unwrapped run_shell failed: out=%q err=%v", out, err)
	}
	if !strings.Contains(out, sandboxMissingNotice) {
		t.Error("unwrapped run should carry the one-time warning")
	}
}

// Guard against a regression in exec arg construction: the wrapped command
// must still be exactly sh -c <command> with nothing dropped.
func TestWrappedArgvKeepsCommandIntact(t *testing.T) {
	const command = "a='b c' && echo \"$a\""
	switch detectShellSandbox() {
	case "sandbox-exec":
		cmd := newShellCommand(command)
		if got := cmd.Args[len(cmd.Args)-1]; got != command {
			t.Fatalf("command mangled: %q", got)
		}
	case "bwrap":
		cmd := newShellCommand(command)
		if got := cmd.Args[len(cmd.Args)-1]; got != command {
			t.Fatalf("command mangled: %q", got)
		}
	default:
		cmd := exec.Command("sh", "-c", command)
		if got := cmd.Args[len(cmd.Args)-1]; got != command {
			t.Fatalf("command mangled: %q", got)
		}
	}
}

// bwrap fails every command when asked to mask a dangling symlink or a path
// through a symlinked directory (/var/run -> /run on Ubuntu), so sockets are
// resolved, deduplicated, and dropped when they do not exist.
func TestResolveSocketsCanonicalizes(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "run")
	if err := os.Mkdir(run, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(run, "docker.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	varRun := filepath.Join(dir, "varrun")
	if err := os.Symlink(run, varRun); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling.sock")
	if err := os.Symlink(filepath.Join(dir, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(sock)
	got := resolveSockets([]string{filepath.Join(varRun, "docker.sock"), sock, dangling, filepath.Join(dir, "absent.sock")})
	if len(got) != 1 || got[0] != want {
		t.Fatalf("resolveSockets = %v, want [%s]", got, want)
	}
}

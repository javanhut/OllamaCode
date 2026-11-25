package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
)

// OS-level confinement for run_shell. Mode gating decides WHETHER a shell
// command may run; this decides WHERE its writes may land. In auto mode the
// gate is fully open, so without this layer every command is `sh -c` with the
// user's full privileges. When the platform provides a sandbox the command is
// wrapped: sandbox-exec (seatbelt) on macOS, bwrap on Linux. Reads and
// process management stay unrestricted; file writes are confined to an
// explicit writable set, so compilers, test runners, and VCS behave as before
// while `rm -rf ~` has nowhere to land.
//
// Network is DENIED by default (loopback stays open, so a test that starts a
// local server still works). Open egress is the exfiltration half of every
// prompt-injection chain: a model steered by a fetched page or a poisoned repo
// file can read a secret it is allowed to read and post it anywhere. Cold
// dependency fetches (go mod download, npm install, git fetch) therefore fail
// inside the sandbox until the user sets shell_network=true; the failing
// result carries a hint saying so (networkBlockedHint).
//
// Some paths INSIDE the writable roots are write-protected (protectedRelPaths):
// git hooks and config, and agent/editor config directories. These are the
// write-then-execute targets behind the 2025-26 agent CVEs — the model writes
// a file that something trusted (git, the editor, the next agent session)
// later runs or obeys, outside any sandbox.
//
// The writable set is the jail roots (workspace + allowlist) plus the
// scratch/cache locations builds genuinely need: $TMPDIR, /tmp,
// /var/folders, the per-user cache dirs (~/Library/Caches, ~/.cache)
// where go-build, compilers, and package managers keep object files, and
// the package-manager/toolchain caches (Go module cache, Cargo, rustup,
// npm) where cold dependency fetches land. Denying those would make every
// build fail inside the sandbox, which is why they are here rather than
// behind the jail. The philosophy stays: package/build caches writable,
// everything else denied.
//
// When neither sandbox binary is on PATH the command runs unwrapped exactly
// as before, and the first run_shell result carries a one-time warning. The
// tools layer cannot see the TUI's mode, so the warning fires whenever a
// command runs unwrapped with the sandbox enabled — one-time per process, so
// it never drowns the transcript. shell_sandbox=false in config disables
// wrapping (and the warning) explicitly.

var (
	shellSandboxOn atomic.Bool
	shellNetworkOn atomic.Bool // false = deny all but loopback inside the sandbox
)

func init() {
	// On by default; config shell_sandbox=false is the kill-switch.
	shellSandboxOn.Store(true)
}

// SetShellSandboxEnabled is the config kill-switch (shell_sandbox, default
// true). Wired from TUI startup; tests can toggle it directly.
func SetShellSandboxEnabled(on bool) { shellSandboxOn.Store(on) }

func shellSandboxEnabled() bool { return shellSandboxOn.Load() }

// SetShellNetworkAllowed is the config switch shell_network (default false).
// It only has effect while the sandbox is on: an unsandboxed shell has
// whatever network the user has.
func SetShellNetworkAllowed(on bool) { shellNetworkOn.Store(on) }

func shellNetworkAllowed() bool { return shellNetworkOn.Load() }

// protectedRelPaths are write-protected inside every writable root, relative
// to that root. Agent and editor config is protected whether or not it exists
// yet (on seatbelt, which can deny a path before it is created): creating
// .vscode/settings.json or .mcp.json is itself the attack. Git paths are
// protected only once they exist, so `git init` in a fresh directory still
// works — after that, hooks and config are fixed.
var protectedRelPaths = []string{
	".ollama_code", ".claude", ".cursor", ".vscode", ".opencode", ".codex", ".gemini",
	".mcp.json",
}

// protectedInstructionNames are instruction files the harness loads as
// standing orders — at the root AND, lazily, in any subdirectory the model
// works in — so they are protected at every depth. Keep in sync with
// internal/instructions.FileNames (tools cannot import it: cycle-free layering).
var protectedInstructionNames = []string{"AGENTS.md", "OLLAMA.md", "CLAUDE.md"}

var protectedGitRelPaths = []string{
	filepath.Join(".git", "hooks"),
	filepath.Join(".git", "config"),
}

// dockerSockets give root-equivalent control of the host to anything that can
// connect to them, sandbox or not.
var dockerSockets = []string{"/var/run/docker.sock", "/run/docker.sock"}

// sandboxProtectedPaths lists the absolute write-protected paths under every
// jail root, raw and symlink-resolved (seatbelt matches canonical paths).
// existingOnly restricts to paths present on disk, which bwrap needs (it can
// only bind over something that exists).
func sandboxProtectedPaths(existingOnly bool) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string, mustExist bool) {
		if seen[p] {
			return
		}
		if mustExist || existingOnly {
			if _, err := os.Lstat(p); err != nil {
				return
			}
		}
		seen[p] = true
		out = append(out, p)
	}
	var roots []string
	for _, root := range jailRoots() {
		roots = append(roots, root)
		if r, err := filepath.EvalSymlinks(root); err == nil && r != root {
			roots = append(roots, r)
		}
	}
	for _, root := range roots {
		for _, rel := range protectedRelPaths {
			add(filepath.Join(root, rel), false)
		}
		// Nested instruction files are covered by seatbeltInstructionRule;
		// here (and so for bwrap) only the root-level ones, which exist.
		for _, name := range protectedInstructionNames {
			add(filepath.Join(root, name), true)
		}
		for _, rel := range protectedGitRelPaths {
			add(filepath.Join(root, rel), true)
		}
	}
	return out
}

// IsProtectedPath reports whether path (absolute or relative to the cwd) is,
// or lies under, one of the write-protected config locations. The file tools
// use it to force an approval prompt even in auto mode: the shell sandbox
// cannot see writes that edit_file makes in-process.
func IsProtectedPath(path string) bool {
	abs := resolveJailPath(path)
	for _, name := range protectedInstructionNames {
		if strings.EqualFold(filepath.Base(abs), name) && withinJail(abs) {
			return true
		}
	}
	for _, root := range jailRoots() {
		for _, rel := range append(append([]string{}, protectedRelPaths...), protectedGitRelPaths...) {
			// Case-folded: macOS and Windows filesystems are case-insensitive,
			// so agents.md IS AGENTS.md. On Linux this can only over-prompt.
			p := strings.ToLower(filepath.Join(root, rel))
			if a := strings.ToLower(abs); a == p || strings.HasPrefix(a, p+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// sandboxKind caches the sandbox probe as a pointer so tests can simulate
// absence: nil = not probed yet, non-nil = probed ("" means none found). A
// racy double-probe is harmless — LookPath is idempotent and cheap.
var sandboxKind atomic.Pointer[string]

// detectShellSandbox returns "sandbox-exec", "bwrap", or "" when no OS
// sandbox is available. Probed once: PATH does not meaningfully change
// mid-process, and run_shell would pay an exec.LookPath per call otherwise.
func detectShellSandbox() string {
	if p := sandboxKind.Load(); p != nil {
		return *p
	}
	kind := ""
	if _, err := exec.LookPath("sandbox-exec"); err == nil {
		kind = "sandbox-exec"
	} else if _, err := exec.LookPath("bwrap"); err == nil {
		kind = "bwrap"
	}
	sandboxKind.Store(&kind)
	return kind
}

// newShellCommand builds `sh -c command`, wrapped in the OS sandbox when one
// is available and enabled. Callers still apply configureShellCommand, so
// process-group kill semantics are identical wrapped or not.
//
// The credential scrub lives here rather than at each caller because this is
// the one place every model-chosen shell is born: a future third spawn site
// cannot forget it. Both sandbox wrappers pass the environment straight
// through, so all three argv shapes are covered.
func newShellCommand(command string) *exec.Cmd {
	argv, extra := shellLaunch(command)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = overrideEnv(scrubbedEnvironment(), extra)
	return cmd
}

// NewShellCommand is newShellCommand for the harness's own spawns, which need a
// deadline. The verification gate and the linter run code the MODEL just wrote
// (`go test ./pkg` over a package whose test file it authored this turn), so a
// test body is a shell the model controls by another route. They get the same
// credential scrub and the same OS sandbox run_shell gets — otherwise the scrub
// is a lock on one door with the next one standing open.
func NewShellCommand(ctx context.Context, command string) *exec.Cmd {
	argv, extra := shellLaunch(command)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = overrideEnv(scrubbedEnvironment(), extra)
	return cmd
}

func shellArgv(command string) []string {
	argv, _ := shellLaunch(command)
	return argv
}

// shellLaunch is shellArgv plus the extra environment the launch needs: the
// proxy variables when egress is routed through the allowlist proxy.
func shellLaunch(command string) (argv, extraEnv []string) {
	if shellSandboxEnabled() {
		kind := detectShellSandbox()
		// Proxy only when allowlisted egress is wanted; if it cannot start,
		// fall through to the deny-all profile rather than opening anything.
		var proxy *netProxy
		if netProxyActive() {
			if p, err := ensureNetProxy(); err == nil {
				proxy = p
			}
		}
		switch kind {
		case "sandbox-exec":
			// The proxy listens on 127.0.0.1, which the loopback-only profile
			// already permits; no profile change is needed.
			profile := seatbeltProfile(sandboxWritableDirs(), sandboxProtectedPaths(false), protectedRoots(), shellNetworkAllowed())
			if proxy != nil {
				extraEnv = proxyEnvironment(proxy.url())
			}
			return []string{"sandbox-exec", "-p", profile, "sh", "-c", command}, extraEnv
		case "bwrap":
			// bwrap bind-mounts must exist, unlike seatbelt subpath rules.
			writable, protected, socks := existingDirs(sandboxWritableDirs()), sandboxProtectedPaths(true), existingSockets()
			if proxy != nil {
				if sock, err := proxy.unixSocket(); err == nil {
					if self, err := os.Executable(); err == nil {
						return bwrapBridgeArgv(command, writable, protected, socks, filepath.Dir(sock), sock, self),
							proxyEnvironment("http://127.0.0.1:" + netBridgePort)
					}
				}
			}
			return bwrapArgv(command, writable, protected, socks, shellNetworkAllowed()), nil
		}
	}
	return []string{"sh", "-c", command}, nil
}

// sandboxWritableDirs is the writeable set for both sandboxes: every jail
// root plus tmp and per-user caches. Each entry is emitted in both its raw
// and symlink-resolved form — seatbelt canonicalizes the paths it matches but
// not the rules, and /var → /private/var on macOS would otherwise slip.
func sandboxWritableDirs() []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	addBoth := func(d string) {
		add(d)
		if r, err := filepath.EvalSymlinks(d); err == nil {
			add(r)
		}
	}
	for _, root := range jailRoots() {
		addBoth(root)
	}
	addBoth(os.TempDir()) // $TMPDIR when set (per-user on macOS), else /tmp
	addBoth("/tmp")
	addBoth("/private/tmp")
	addBoth("/var/folders")
	addBoth("/private/var/folders")
	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		addBoth(filepath.Join(home, "Library", "Caches")) // go-build & friends, macOS
		addBoth(filepath.Join(home, ".cache"))            // go-build & friends, Linux
	}
	// Package-manager/toolchain caches: cold dependency fetches (go mod
	// download, cargo build, npm install) write here rather than under the
	// workspace, so a sandboxed first build fails without them. Each lookup
	// is env-aware because the tool lets the user relocate its cache, and
	// each dir is added only when it already exists: the package manager can
	// extend an existing cache under the sandbox but cannot create one (HOME
	// itself stays read-only), and bwrap bind-mounts must exist.
	addCache := func(d string) {
		if d == "" {
			return
		}
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			addBoth(d)
		}
	}
	switch gomod := os.Getenv("GOMODCACHE"); {
	case gomod != "":
		addCache(gomod)
	case os.Getenv("GOPATH") != "":
		// go keeps the module cache under the first GOPATH entry.
		addCache(filepath.Join(filepath.SplitList(os.Getenv("GOPATH"))[0], "pkg", "mod"))
	case homeErr == nil:
		addCache(filepath.Join(home, "go", "pkg", "mod"))
	}
	switch cargo := os.Getenv("CARGO_HOME"); {
	case cargo != "":
		addCache(cargo)
	case homeErr == nil:
		addCache(filepath.Join(home, ".cargo"))
	}
	if homeErr == nil {
		addCache(filepath.Join(home, ".rustup"))
	}
	switch npm := os.Getenv("NPM_CONFIG_CACHE"); {
	case npm != "":
		addCache(npm)
	case homeErr == nil:
		addCache(filepath.Join(home, ".npm"))
	}
	return dirs
}

func existingDirs(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// withinJail reports whether an already-resolved path lies under a jail root.
func withinJail(abs string) bool {
	for _, root := range jailRoots() {
		if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// seatbeltInstructionRule denies writes to an instruction file at any depth
// under the writable jail roots. Seatbelt matches the canonical path, which on
// a case-insensitive volume also catches agents.md (verified).
func seatbeltInstructionRule(roots []string) string {
	names := make([]string, len(protectedInstructionNames))
	for i, n := range protectedInstructionNames {
		names[i] = regexp.QuoteMeta(n)
	}
	var b strings.Builder
	for _, root := range roots {
		if strings.ContainsAny(root, "\"\\") {
			continue // cannot be expressed safely inside #"..."; root-level files are still literal-denied
		}
		fmt.Fprintf(&b, "(deny file-write* (regex #\"^%s/(.*/)?(%s)$\"))\n", regexp.QuoteMeta(root), strings.Join(names, "|"))
	}
	return b.String()
}

// protectedRoots is every jail root in raw and symlink-resolved form.
func protectedRoots() []string {
	var out []string
	for _, root := range jailRoots() {
		out = append(out, root)
		if r, err := filepath.EvalSymlinks(root); err == nil && r != root {
			out = append(out, r)
		}
	}
	return out
}

func existingSockets() []string {
	var out []string
	for _, s := range dockerSockets {
		if _, err := os.Lstat(s); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// seatbeltProfile renders the macOS sandbox-exec profile. Pure, so tests can
// pin the shape without sandbox-exec. Seatbelt applies the LAST matching rule,
// so every deny that carves an exception out of an allow comes after it.
func seatbeltProfile(writable, protected, instructionRoots []string, network bool) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	// Default-allow keeps reads, process spawn/signal, and mach services
	// working; writes and (by default) network are walled off below.
	b.WriteString("(allow default)\n(deny file-write*)\n")
	for _, dir := range writable {
		fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", sbplString(dir))
	}
	// Device sinks shell pipelines write to constantly (2>/dev/null & co.).
	for _, dev := range []string{"/dev/null", "/dev/zero", "/dev/tty", "/dev/ptmx", "/dev/random", "/dev/urandom"} {
		fmt.Fprintf(&b, "(allow file-write* (literal %s))\n", sbplString(dev))
	}
	for _, p := range protected {
		fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", sbplString(p))
	}
	b.WriteString(seatbeltInstructionRule(instructionRoots))
	if !network {
		// Loopback only. The three narrow rules are deliberate: a broad
		// (allow network* (local ip "localhost:*")) also matches OUTBOUND
		// sockets and silently re-opens egress (verified empirically).
		// Unix-domain sockets stay denied too, which covers the Docker
		// socket. DNS on macOS goes through mDNSResponder over mach, not a
		// socket, so the lookup service is denied as well — DNS is an
		// exfiltration channel on its own.
		b.WriteString("(deny network*)\n")
		b.WriteString(`(allow network-outbound (remote ip "localhost:*"))` + "\n")
		b.WriteString(`(allow network-bind (local ip "localhost:*"))` + "\n")
		b.WriteString(`(allow network-inbound (local ip "localhost:*"))` + "\n")
		b.WriteString(`(deny mach-lookup (global-name "com.apple.dnssd.service"))` + "\n")
	} else {
		for _, sock := range dockerSockets {
			fmt.Fprintf(&b, "(deny network-outbound (remote unix-socket (path-literal %s)))\n", sbplString(sock))
			fmt.Fprintf(&b, "(deny network-outbound (remote unix-socket (path-literal %s)))\n", sbplString("/private"+sock))
		}
	}
	return b.String()
}

func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// bwrapArgv renders the Linux bubblewrap invocation: the host filesystem
// read-only, the writable roots overlaid read-write, the protected paths
// re-bound read-only on top (later mounts win), the Docker socket masked, and
// — unless network is allowed — a fresh network namespace that has only
// loopback. Pure, so tests can pin the shape.
func bwrapArgv(command string, writable, protected, sockets []string, network bool) []string {
	argv := []string{
		"bwrap", "--die-with-parent",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}
	if !network {
		argv = append(argv, "--unshare-net")
	}
	for _, dir := range writable {
		argv = append(argv, "--bind", dir, dir)
	}
	for _, p := range protected {
		argv = append(argv, "--ro-bind", p, p)
	}
	// Unix sockets are path-addressed, so a new network namespace does not
	// hide them; bind /dev/null over the path instead.
	for _, sock := range sockets {
		argv = append(argv, "--ro-bind", "/dev/null", sock)
	}
	return append(argv, "--", "sh", "-c", command)
}

// bwrapBridgeArgv is bwrapArgv with a private network namespace plus the
// proxy bridge: the socket dir is bound in, and the ocode binary runs first
// inside the sandbox as the loopback forwarder (see netbridge.go), then runs
// the command. The forwarder's own argv is the command's argv.
func bwrapBridgeArgv(command string, writable, protected, sockets []string, sockDir, sock, self string) []string {
	argv := bwrapArgv(command, writable, protected, sockets, false)
	argv = argv[:len(argv)-4] // drop "-- sh -c <command>"
	return append(argv,
		"--bind", sockDir, sockDir,
		"--setenv", netBridgeEnvFlag, "1",
		"--setenv", netBridgeEnvSock, sock,
		"--", self, "sh", "-c", command)
}

// sandboxMissingNotice rides the first unwrapped run_shell result so both the
// user (transcript) and the model (context) learn the sandbox is off.
const sandboxMissingNotice = "[warning: no OS sandbox found (neither sandbox-exec nor bwrap on PATH) — this command ran UNSANDBOXED with your full user privileges. Path arguments of file tools are still jailed to the workspace, but shell commands can write anywhere. Install bubblewrap, or set shell_sandbox=false in config to dismiss this warning.]"

var sandboxNoticeGiven atomic.Bool

// networkFailureMarkers are the error texts common tools print when a
// connection or name lookup fails. Matched case-insensitively.
var networkFailureMarkers = []string{
	"could not resolve host", "no such host", "temporary failure in name resolution",
	"name or service not known", "nodename nor servname", "getaddrinfo", "enotfound",
	"network is unreachable", "eai_again",
	// Deliberately absent: "connection refused" and "dial tcp", which a test
	// against its own (stopped) localhost server prints too — loopback is open,
	// so those are real failures, not policy.
}

// networkBlockedHint is appended to a shell result that looks like it failed
// on network access while the sandbox was blocking it, so neither the model
// nor the user mistakes a policy denial for a broken registry or a typo.
const networkBlockedHint = "[note: network access is blocked inside the shell sandbox (only localhost is reachable). This failure looks network-related. Do not retry or look for workarounds; tell the user. They can allow it with \"shell_network\": true in config.json, or run the command themselves.]"

// allowlistBlockedHint is the variant for when shell_network_allow is active:
// only the listed domains are reachable, so the fix is one more list entry,
// not opening the network.
func allowlistBlockedHint() string {
	var b strings.Builder
	b.WriteString("[note: network egress inside the shell sandbox is limited to an allowlist of domains (shell_network_allow).")
	if hosts := deniedHostsNote(); hosts != "" {
		b.WriteString(" Blocked host(s): " + hosts + ".")
	}
	b.WriteString(" This failure looks network-related. Do not retry or look for workarounds; tell the user. They can allow the domain by adding it to \"shell_network_allow\" (a list of domains, e.g. [\"pypi.org\", \"*.githubusercontent.com\"]) in config.json, or run the command themselves.]")
	return b.String()
}

// withNetworkHint appends the blocked-network hint when the sandbox is
// actually blocking network and the output carries a network failure marker.
func withNetworkHint(result string) string {
	if !shellSandboxEnabled() || shellNetworkAllowed() || detectShellSandbox() == "" {
		return result
	}
	hint := networkBlockedHint
	markers := networkFailureMarkers
	if len(currentAllowlist()) > 0 {
		hint = allowlistBlockedHint()
		markers = append(append([]string{}, markers...), proxyDenialMarkers...)
	}
	lower := strings.ToLower(result)
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return result + "\n" + hint
		}
	}
	return result
}

// proxyDenialMarkers are what tools print when the allowlist proxy refuses a
// request (our 403 body, curl's CONNECT failure, npm/pip proxy errors).
var proxyDenialMarkers = []string{
	"denied by shell_network_allow", "403 from proxy", "proxy connect aborted",
	"tunnel connection failed", "connect tunnel failed", "proxyerror",
}

// withSandboxNotice prepends the missing-sandbox warning exactly once per
// process, and only when wrapping was wanted (enabled) but impossible.
func withSandboxNotice(result string) string {
	if !shellSandboxEnabled() || detectShellSandbox() != "" {
		return result
	}
	if !sandboxNoticeGiven.CompareAndSwap(false, true) {
		return result
	}
	return sandboxMissingNotice + "\n" + result
}

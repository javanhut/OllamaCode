package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// OS-level confinement for run_shell. Mode gating decides WHETHER a shell
// command may run; this decides WHERE its writes may land. In auto mode the
// gate is fully open, so without this layer every command is `sh -c` with the
// user's full privileges. When the platform provides a sandbox the command is
// wrapped: sandbox-exec (seatbelt) on macOS, bwrap on Linux. Reads, process
// management, and network stay unrestricted — the sandbox only denies file
// writes outside an explicit writable set, so compilers, test runners, and
// VCS behave exactly as before while `rm -rf ~` has nowhere to land.
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

var shellSandboxOn atomic.Bool

func init() {
	// On by default; config shell_sandbox=false is the kill-switch.
	shellSandboxOn.Store(true)
}

// SetShellSandboxEnabled is the config kill-switch (shell_sandbox, default
// true). Wired from TUI startup; tests can toggle it directly.
func SetShellSandboxEnabled(on bool) { shellSandboxOn.Store(on) }

func shellSandboxEnabled() bool { return shellSandboxOn.Load() }

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
func newShellCommand(command string) *exec.Cmd {
	if shellSandboxEnabled() {
		switch detectShellSandbox() {
		case "sandbox-exec":
			return exec.Command("sandbox-exec", "-p", seatbeltProfile(sandboxWritableDirs()), "sh", "-c", command)
		case "bwrap":
			// bwrap bind-mounts must exist, unlike seatbelt subpath rules.
			argv := bwrapArgv(command, existingDirs(sandboxWritableDirs()))
			return exec.Command(argv[0], argv[1:]...)
		}
	}
	return exec.Command("sh", "-c", command)
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

// seatbeltProfile renders the macOS sandbox-exec profile for the given
// writable roots. Pure, so tests can pin the shape without sandbox-exec.
func seatbeltProfile(writable []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	// Default-allow keeps reads, process spawn/signal, mach services, and the
	// network working — the sandbox only walls off where output can land.
	b.WriteString("(allow default)\n(deny file-write*)\n")
	for _, dir := range writable {
		fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", sbplString(dir))
	}
	// Device sinks shell pipelines write to constantly (2>/dev/null & co.).
	for _, dev := range []string{"/dev/null", "/dev/zero", "/dev/tty", "/dev/ptmx", "/dev/random", "/dev/urandom"} {
		fmt.Fprintf(&b, "(allow file-write* (literal %s))\n", sbplString(dev))
	}
	return b.String()
}

func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// bwrapArgv renders the Linux bubblewrap invocation: the host filesystem
// read-only, the writable roots overlaid read-write, scratch /dev and /proc,
// network left shared (not unshared). Pure, so tests can pin the shape.
func bwrapArgv(command string, writable []string) []string {
	argv := []string{
		"bwrap", "--die-with-parent",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}
	for _, dir := range writable {
		argv = append(argv, "--bind", dir, dir)
	}
	return append(argv, "--", "sh", "-c", command)
}

// sandboxMissingNotice rides the first unwrapped run_shell result so both the
// user (transcript) and the model (context) learn the sandbox is off.
const sandboxMissingNotice = "[warning: no OS sandbox found (neither sandbox-exec nor bwrap on PATH) — this command ran UNSANDBOXED with your full user privileges. Path arguments of file tools are still jailed to the workspace, but shell commands can write anywhere. Install bubblewrap, or set shell_sandbox=false in config to dismiss this warning.]"

var sandboxNoticeGiven atomic.Bool

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

package tools

import (
	"os"
	"sync"
)

// Oversized tool output used to be destroyed: truncateResult kept a head and a
// tail and threw the middle away permanently, so the model could not get it
// back at any price — not by asking, not by re-running a command that had
// already had its side effect. Spilling writes the FULL text to a private file
// and puts the path in the envelope, so the elided middle is one read_file or
// grep away.
//
// Storage is best effort by construction: every failure path returns ok=false
// and the caller keeps today's inline truncation. A full disk must never turn a
// successful tool call into an error.
//
// os.MkdirTemp/os.CreateTemp are exactly the primitives wanted here, so there
// is nothing hand-rolled: MkdirTemp makes a random-named 0700 dir under $TMPDIR
// (per-user on macOS), CreateTemp opens a random name with O_EXCL at 0600. The
// O_EXCL-inside-a-0700-dir combination is what defeats a planted symlink; the
// name randomness is not a security boundary on its own.

// spillBudget caps what one process may write, across all spills, so a runaway
// session cannot fill the disk. 32 MB is roughly 2700 max-size inline results.
// CleanupSpills handles the ordinary exit; the budget still matters because a
// crash skips it and $TMPDIR reaping is measured in days.
const spillBudget = 32 * 1024 * 1024

var spillState struct {
	sync.Mutex
	dir   string
	bytes int64
}

// spillResult writes text to a fresh private file and returns its path. ok is
// false whenever anything at all went wrong, which means "keep the truncated
// result exactly as it is today".
func spillResult(text string) (string, bool) {
	spillState.Lock()
	defer spillState.Unlock()

	if spillState.bytes+int64(len(text)) > spillBudget {
		return "", false
	}
	dir, err := spillDirLocked()
	if err != nil {
		return "", false
	}
	f, err := os.CreateTemp(dir, "result-*.txt")
	if err != nil {
		return "", false
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", false
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", false
	}
	spillState.bytes += int64(len(text))
	return f.Name(), true
}

// spillDirLocked returns the session spill dir, creating it on first use. The
// Stat re-check is not paranoia: a tmp reaper (or a test's t.TempDir cleanup
// when TMPDIR was redirected) can delete the dir mid-session, and remaking it
// is cheaper than silently losing every later spill.
func spillDirLocked() (string, error) {
	if spillState.dir != "" {
		if info, err := os.Stat(spillState.dir); err == nil && info.IsDir() {
			return spillState.dir, nil
		}
		spillState.dir = ""
	}
	dir, err := os.MkdirTemp("", "ocode-spill-")
	if err != nil {
		return "", err
	}
	// Store the symlink-resolved form so the path handed to the model matches
	// the jail's containment check byte for byte (/var → /private/var on macOS).
	spillState.dir = resolveJailPath(dir)
	return spillState.dir, nil
}

// CleanupSpills removes this process's spill directory. Spilled text is tool
// output — file contents, git history, command output — sitting in plaintext
// outside the workspace, so an ordinary exit should not leave it for a reaper
// whose timer is measured in days. A crash still does; nothing to be done about
// that from in here.
func CleanupSpills() {
	spillState.Lock()
	defer spillState.Unlock()
	if spillState.dir == "" {
		return
	}
	_ = os.RemoveAll(spillState.dir)
	spillState.dir, spillState.bytes = "", 0
}

// spillDirIfCreated reports the spill dir, or "" when nothing has spilled yet.
// Read-only on purpose: jailCheck calls it on every filesystem tool call and
// must never create a directory as a side effect of a containment test.
func spillDirIfCreated() string {
	spillState.Lock()
	defer spillState.Unlock()
	return spillState.dir
}

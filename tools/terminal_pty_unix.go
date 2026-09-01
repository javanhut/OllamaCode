//go:build unix

package tools

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// A pty is wider than a real terminal on purpose: at 80 columns the kernel line
// discipline wraps mid-token, and the model then reads broken identifiers and
// paths out of the buffer.
const (
	terminalRows uint16 = 24
	terminalCols uint16 = 200
)

// startPTY opens a pty pair, makes the child its controlling terminal, and
// starts it. pty.StartWithSize sets SysProcAttr.Setsid and Setctty itself,
// which is why configureShellCommand (Setpgid) is deliberately NOT applied to a
// terminal command: Setsid together with Setpgid is rejected by the kernel
// ("fork/exec /bin/sh: operation not permitted"), so adding the call that every
// other spawn site in this package makes would break every terminal_open.
// Nothing is lost by its absence — Setsid already makes the child a session
// leader whose pgid equals its pid, so killShellCommand's Kill(-pid) still
// reaches the whole tree.
func startPTY(cmd *exec.Cmd) (*os.File, error) {
	return pty.StartWithSize(cmd, &pty.Winsize{Rows: terminalRows, Cols: terminalCols})
}

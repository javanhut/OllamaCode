//go:build !unix

package tools

import (
	"errors"
	"os"
	"os/exec"
)

// creack/pty has no Windows implementation, and emulating one over ConPTY is
// not worth it for a tool run_shell already substitutes for.
func startPTY(cmd *exec.Cmd) (*os.File, error) {
	return nil, errors.New("persistent terminal sessions are not supported on this platform (no pty); use run_shell instead")
}

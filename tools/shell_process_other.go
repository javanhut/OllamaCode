//go:build !unix

package tools

import "os/exec"

func configureShellCommand(cmd *exec.Cmd) {}

func killShellCommand(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

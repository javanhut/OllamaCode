//go:build !unix

package mcp

import "os/exec"

func configureShellCommand(cmd *exec.Cmd) {}

func killShellCommand(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

//go:build windows

package mcp

import "os/exec"

func configureProcess(_ *exec.Cmd) {}

func terminateProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

const terminalPath = "CONIN$"

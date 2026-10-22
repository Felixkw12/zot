//go:build windows

package mcp

import "os/exec"

func configureProcess(_ *exec.Cmd) {}

// Windows has no interrupt signal for a console-less child. The closed
// stdin is the only graceful signal, after which the process is killed.
func interruptProcess(cmd *exec.Cmd) { terminateProcess(cmd) }

func terminateProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

const terminalPath = "CONIN$"

//go:build !windows

package mcp

import (
	"os"
	"os/signal"
	"syscall"
)

// ignoreInterrupt makes the stubborn helper survive SIGTERM.
func ignoreInterrupt() {
	signal.Ignore(syscall.SIGTERM)
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM)
}

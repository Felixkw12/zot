//go:build windows

package mcp

// ignoreInterrupt is a no-op, Windows servers receive no interrupt.
func ignoreInterrupt() {}

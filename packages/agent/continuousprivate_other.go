//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package agent

import (
	"fmt"
	"os"
)

// Private file and lock checks fail closed on platforms without a
// supported implementation.

func openPrivateFile(string) (*os.File, error) {
	return nil, fmt.Errorf("private file checks are unsupported on this platform")
}

func openPrivateCreate(string) (*os.File, error) {
	return nil, fmt.Errorf("private file checks are unsupported on this platform")
}

func createPrivateExclusive(string) (*os.File, error) {
	return nil, fmt.Errorf("private file checks are unsupported on this platform")
}

func lockStateFile(*os.File) error {
	return fmt.Errorf("state directory locking is unsupported on this platform")
}

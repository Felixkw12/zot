//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package agent

import (
	"errors"
	"fmt"
	"os"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"golang.org/x/sys/unix"
)

// openPrivateFile opens path without following a final symlink and checks
// the opened handle: a regular file owned by the effective user with no
// group or other permission bits. These are operational guardrails, not a
// boundary against a hostile process running as the same user.
func openPrivateFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("must not be a symlink")
		}
		return nil, fmt.Errorf("cannot open: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if err := checkPrivateHandle(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func checkPrivateHandle(f *os.File) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return fmt.Errorf("cannot inspect: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("must be a regular file")
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("must be owned by the current user")
	}
	if st.Mode&0o077 != 0 {
		return fmt.Errorf("must not be group or world accessible (mode %o)", st.Mode&0o777)
	}
	return nil
}

// openPrivateCreate opens or creates a private regular file for the state
// directory without following symlinks, then validates the opened handle.
func openPrivateCreate(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%s must not be a symlink", path)
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err := checkPrivateHandle(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s %w", path, err)
	}
	return f, nil
}

// createPrivateExclusive creates a new mode 0600 file, failing if any
// file or symlink already exists at path.
func createPrivateExclusive(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// lockStateFile takes a nonblocking exclusive lock held for the lifetime
// of f. Contention reports storage.ErrLocked.
func lockStateFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return storage.ErrLocked
	}
	return err
}

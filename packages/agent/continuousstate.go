package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/sqlite"
)

// Local state directory files for a remote-backed host.
const (
	stateLockFile     = "state.lock"
	stateIdentityFile = "store.json"
)

// stateIdentity binds a local state directory to one remote authority.
type stateIdentity struct {
	Version   int    `json:"version"`
	Backend   string `json:"backend"`
	StoreUUID string `json:"store_uuid"`
}

// hostState is a locked local state directory. The lock protects local
// endpoint ownership (socket, token) only: it is not a substitute for the
// database writer lock, and it must live on a local filesystem.
type hostState struct {
	dir  string
	lock *os.File
}

// acquireHostState creates or validates dir and takes its exclusive lock.
// It refuses directories holding local store authority, so a journal or
// sqlite store is never shadowed by a remote host.
func acquireHostState(dir string) (*hostState, error) {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("state directory %s must be a real directory", dir)
	}
	for _, name := range []string{"boundary", sqlite.DatabaseFile} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return nil, fmt.Errorf("state directory %s contains a local continuous store (%s), use a separate directory", dir, name)
		}
	}
	lock, err := openPrivateCreate(filepath.Join(dir, stateLockFile))
	if err != nil {
		return nil, fmt.Errorf("state lock: %w", err)
	}
	if err := lockStateFile(lock); err != nil {
		lock.Close()
		if errors.Is(err, storage.ErrLocked) {
			return nil, fmt.Errorf("state directory %s is in use by another host: %w", dir, storage.ErrLocked)
		}
		return nil, fmt.Errorf("lock state directory: %w", err)
	}
	return &hostState{dir: dir, lock: lock}, nil
}

// bind publishes the store identity if absent, or requires an exact match.
// A conflicting identity is never overwritten.
func (h *hostState) bind(backend, storeUUID string) error {
	want := stateIdentity{Version: 1, Backend: backend, StoreUUID: storeUUID}
	path := filepath.Join(h.dir, stateIdentityFile)
	f, err := openPrivateFile(path)
	if err == nil {
		defer f.Close()
		var got stateIdentity
		dec := json.NewDecoder(f)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&got); err != nil {
			return fmt.Errorf("state identity %s is unreadable, refusing to replace it", path)
		}
		if got.Version != 1 {
			return fmt.Errorf("state identity %s has unsupported version %d", path, got.Version)
		}
		if got != want {
			return fmt.Errorf("state directory %s belongs to %s store %s, not %s, use a separate --state-dir", h.dir, got.Backend, got.StoreUUID, storeUUID)
		}
		return nil
	}
	if _, statErr := os.Lstat(path); statErr == nil {
		return fmt.Errorf("state identity %s: %w", path, err)
	}
	data, err := json.Marshal(want)
	if err != nil {
		return err
	}
	// Publish atomically: write a private temporary file, then link it into
	// place without replacing an identity created meanwhile.
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmpName := filepath.Join(h.dir, ".store-"+hex.EncodeToString(suffix[:])+".json")
	tmp, err := createPrivateExclusive(tmpName)
	if err != nil {
		return err
	}
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return h.bind(backend, storeUUID)
		}
		return fmt.Errorf("publish state identity: %w", err)
	}
	return nil
}

func (h *hostState) path(name string) string { return filepath.Join(h.dir, name) }

func (h *hostState) close() error {
	if h == nil || h.lock == nil {
		return nil
	}
	err := h.lock.Close()
	h.lock = nil
	return err
}

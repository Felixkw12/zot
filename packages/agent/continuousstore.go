package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/continuous/storage/postgres"
	"github.com/patriceckhart/zot/packages/continuous/storage/sqlite"
)

// Store backends selectable with --backend. The journal is the default for
// new local stores. Existing local stores are opened with the backend they
// were created with, detected from their files. PostgreSQL is never
// detected or defaulted: it requires --backend postgres and --store-name.
const (
	backendJournal  = "journal"
	backendSQLite   = "sqlite"
	backendPostgres = "postgres"
)

// continuousPostgresDSNEnv is the DSN fallback when no --postgres-dsn-file
// is given. It never selects the backend by itself.
const continuousPostgresDSNEnv = "ZOT_CONTINUOUS_POSTGRES_DSN"

const (
	minPostgresOperationTimeout = time.Second
	maxPostgresOperationTimeout = 15 * time.Minute
)

// continuousTarget is the storage authority a command opens: a local
// directory or a PostgreSQL logical store. Exactly one form is set.
type continuousTarget struct {
	store      string
	backend    string
	durability storage.Durability

	storeName        string
	dsnFile          string
	operationTimeout time.Duration
	stateDir         string

	// durabilitySet records an explicit --durability.
	durabilitySet bool
}

func (t continuousTarget) postgres() bool { return t.backend == backendPostgres }

// label is a safe display name: never a DSN.
func (t continuousTarget) label() string {
	if t.postgres() {
		return "postgres store " + t.storeName
	}
	return t.store
}

// storageFlag consumes one storage flag at args[i] into t, returning the
// index of its last argument. ok is false for flags it does not own.
// Duplicates and empty values are errors in every parser.
func (t *continuousTarget) storageFlag(args []string, i int, seen map[string]bool) (next int, ok bool, err error) {
	arg := args[i]
	switch arg {
	case "--store", "--backend", "--durability", "--store-name", "--postgres-dsn-file", "--postgres-operation-timeout", "--state-dir":
	default:
		return i, false, nil
	}
	if seen[arg] {
		return i, true, fmt.Errorf("duplicate continuous flag: %s", arg)
	}
	seen[arg] = true
	if i+1 >= len(args) || args[i+1] == "" || len(args[i+1]) > 1 && args[i+1][:2] == "--" {
		return i, true, fmt.Errorf("%s requires a value", arg)
	}
	value := args[i+1]
	switch arg {
	case "--store":
		t.store = value
	case "--backend":
		if !validContinuousBackend(value) {
			return i, true, fmt.Errorf("backend must be journal, sqlite, or postgres")
		}
		t.backend = value
	case "--durability":
		if value != "strict" && value != "process" {
			return i, true, fmt.Errorf("durability must be strict or process")
		}
		t.durability, t.durabilitySet = continuousDurability(value), true
	case "--store-name":
		if !postgres.ValidStoreName(value) {
			return i, true, fmt.Errorf("--store-name must match [A-Za-z0-9][A-Za-z0-9._-]* and be at most 128 characters")
		}
		t.storeName = value
	case "--postgres-dsn-file":
		t.dsnFile = value
	case "--postgres-operation-timeout":
		d, err := time.ParseDuration(value)
		if err != nil || d < minPostgresOperationTimeout || d > maxPostgresOperationTimeout {
			return i, true, fmt.Errorf("--postgres-operation-timeout must be a duration from 1s to 15m")
		}
		t.operationTimeout = d
	case "--state-dir":
		t.stateDir = value
	}
	return i + 1, true, nil
}

// validate checks the combination of storage flags. serve requires and
// allows --state-dir for PostgreSQL.
func (t *continuousTarget) validate(command string, serve bool) error {
	if t.postgres() {
		switch {
		case t.store != "":
			return fmt.Errorf("--store is a local directory and cannot be combined with --backend postgres")
		case t.storeName == "":
			return fmt.Errorf("--backend postgres requires --store-name")
		case t.durabilitySet && t.durability != storage.Strict:
			return fmt.Errorf("--backend postgres supports only --durability strict")
		case serve && t.stateDir == "":
			return fmt.Errorf("continuous serve with --backend postgres requires --state-dir for local host state")
		case !serve && t.stateDir != "":
			return fmt.Errorf("--state-dir applies only to continuous serve with --backend postgres")
		}
		t.durability = storage.Strict
		return nil
	}
	for flag, set := range map[string]bool{"--store-name": t.storeName != "", "--postgres-dsn-file": t.dsnFile != "", "--postgres-operation-timeout": t.operationTimeout != 0, "--state-dir": t.stateDir != ""} {
		if set {
			return fmt.Errorf("%s requires --backend postgres", flag)
		}
	}
	if t.store == "" {
		return fmt.Errorf("continuous %s requires --store <directory>, or --backend postgres --store-name <name>", command)
	}
	return nil
}

// detectContinuousBackend reports which backend created the store at path,
// or "" for a directory that is not yet a store. A directory containing
// files of both backends is ambiguous and refused.
func detectContinuousBackend(path string) (string, error) {
	_, journalErr := os.Stat(filepath.Join(path, "boundary"))
	_, sqliteErr := os.Stat(filepath.Join(path, sqlite.DatabaseFile))
	switch {
	case journalErr == nil && sqliteErr == nil:
		return "", fmt.Errorf("continuous store %s contains both journal and sqlite files", path)
	case journalErr == nil:
		return backendJournal, nil
	case sqliteErr == nil:
		return backendSQLite, nil
	}
	return "", nil
}

// openContinuousStore opens the store at path. An explicit backend must
// match an existing store; an empty backend uses the detected one, or the
// journal for a new store.
func openContinuousStore(ctx context.Context, path, backend string, durability storage.Durability) (storage.Store, error) {
	existing, err := detectContinuousBackend(path)
	if err != nil {
		return nil, err
	}
	switch {
	case backend == "":
		backend = existing
		if backend == "" {
			backend = backendJournal
		}
	case existing != "" && existing != backend:
		return nil, fmt.Errorf("continuous store %s is a %s store, not %s", path, existing, backend)
	}
	switch backend {
	case backendJournal:
		return journal.Open(ctx, path, journal.Options{Durability: durability})
	case backendSQLite:
		return sqlite.Open(ctx, path, sqlite.Options{Durability: durability})
	}
	return nil, errors.New("backend must be journal or sqlite")
}

// openContinuousTarget opens a resolved target. create allows a missing
// PostgreSQL store to be created. Local stores keep their own rules.
func openContinuousTarget(ctx context.Context, t continuousTarget, create bool) (storage.Store, error) {
	if !t.postgres() {
		return openContinuousStore(ctx, t.store, t.backend, t.durability)
	}
	dsn, err := loadPostgresDSN(t.dsnFile)
	if err != nil {
		return nil, err
	}
	s, err := postgres.Open(ctx, dsn, postgres.Options{StoreName: t.storeName, Create: create, Durability: storage.Strict, OperationTimeout: t.operationTimeout})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// loadPostgresDSN reads the explicit DSN file, or the environment fallback
// when no file was selected. A selected file that cannot be used is an
// error, never a reason to fall back.
func loadPostgresDSN(file string) (string, error) {
	if file != "" {
		return readPostgresDSNFile(file)
	}
	dsn := os.Getenv(continuousPostgresDSNEnv)
	if dsn == "" {
		return "", fmt.Errorf("--backend postgres requires --postgres-dsn-file or %s", continuousPostgresDSNEnv)
	}
	if err := postgres.ValidateDSNText(dsn); err != nil {
		return "", fmt.Errorf("%s: %w", continuousPostgresDSNEnv, err)
	}
	return dsn, nil
}

// readPostgresDSNFile reads a private regular file holding one connection
// string. One trailing LF or CRLF is removed. Contents never appear in
// errors.
func readPostgresDSNFile(path string) (string, error) {
	f, err := openPrivateFile(path)
	if err != nil {
		return "", fmt.Errorf("postgres DSN file %s: %w", path, err)
	}
	defer f.Close()
	buf := make([]byte, postgres.MaxDSNBytes+3)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("postgres DSN file %s: read failed", path)
	}
	data := buf[:n]
	switch {
	case len(data) >= 2 && string(data[len(data)-2:]) == "\r\n":
		data = data[:len(data)-2]
	case len(data) >= 1 && data[len(data)-1] == '\n':
		data = data[:len(data)-1]
	}
	if err := postgres.ValidateDSNText(string(data)); err != nil {
		return "", fmt.Errorf("postgres DSN file %s: %w", path, err)
	}
	return string(data), nil
}

// validContinuousBackend reports whether name is a selectable backend.
func validContinuousBackend(name string) bool {
	return name == backendJournal || name == backendSQLite || name == backendPostgres
}

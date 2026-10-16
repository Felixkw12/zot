package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestContinuousPostgresTargetParsing(t *testing.T) {
	pg := []string{"--backend", "postgres", "--store-name", "main"}
	cases := []struct {
		name string
		args []string
		err  string
	}{
		{"run ok", append([]string{"hi"}, pg...), ""},
		{"run resume ok", append([]string{"--resume"}, pg...), ""},
		{"store and postgres", append([]string{"hi", "--store", "x"}, pg...), "cannot be combined"},
		{"missing name", []string{"hi", "--backend", "postgres"}, "requires --store-name"},
		{"name without postgres", []string{"hi", "--store", "x", "--store-name", "main"}, "requires --backend postgres"},
		{"dsn file without postgres", []string{"hi", "--store", "x", "--postgres-dsn-file", "f"}, "requires --backend postgres"},
		{"process durability", append([]string{"hi", "--durability", "process"}, pg...), "only --durability strict"},
		{"state dir on run", append([]string{"hi", "--state-dir", "d"}, pg...), "only to continuous serve"},
		{"duplicate", append([]string{"hi", "--store-name", "b"}, pg...), "duplicate"},
		{"bad name", []string{"hi", "--backend", "postgres", "--store-name", "-x"}, "--store-name must match"},
		{"timeout zero", append([]string{"hi", "--postgres-operation-timeout", "0s"}, pg...), "1s to 15m"},
		{"timeout high", append([]string{"hi", "--postgres-operation-timeout", "16m"}, pg...), "1s to 15m"},
		{"empty value", []string{"hi", "--backend", "postgres", "--store-name", ""}, "requires a value"},
		{"local unchanged", []string{"hi", "--store", "x"}, ""},
		{"trust durability ok", append([]string{"hi", "--postgres-trust-remote-durability"}, pg...), ""},
		{"trust durability local", []string{"hi", "--store", "x", "--postgres-trust-remote-durability"}, "requires --backend postgres"},
		{"trust durability twice", append([]string{"hi", "--postgres-trust-remote-durability", "--postgres-trust-remote-durability"}, pg...), "duplicate"},
	}
	for _, c := range cases {
		opts, err := parseContinuousRunArgs(c.args)
		if c.err == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			if len(opts.hostArgs) != 0 {
				t.Errorf("%s: storage flags leaked to host args: %v", c.name, opts.hostArgs)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.err)
		}
	}
	opts, err := parseContinuousRunArgs(append([]string{"hi", "--postgres-operation-timeout", "2m", "--model", "m"}, pg...))
	if err != nil || opts.target.operationTimeout.String() != "2m0s" || strings.Join(opts.hostArgs, " ") != "--model m" {
		t.Fatalf("timeout and host args: %+v %v", opts, err)
	}
}

func TestContinuousPostgresServeParsing(t *testing.T) {
	if _, err := parseContinuousServeArgs([]string{"--backend", "postgres", "--store-name", "main"}); err == nil || !strings.Contains(err.Error(), "--state-dir") {
		t.Fatalf("serve without state dir: %v", err)
	}
	dir := t.TempDir()
	opts, err := parseContinuousServeArgs([]string{"--backend", "postgres", "--store-name", "main", "--state-dir", dir})
	if err != nil {
		t.Fatal(err)
	}
	if opts.socket != filepath.Join(dir, "host.sock") || opts.store != dir {
		t.Fatalf("local endpoints not in state dir: %+v", opts)
	}
	if _, err := parseContinuousServeArgs([]string{"--store", dir, "--state-dir", dir}); err == nil {
		t.Fatal("--state-dir accepted for a local backend")
	}
}

func TestContinuousPostgresCommandSupport(t *testing.T) {
	pg := []string{"--backend", "postgres", "--store-name", "main"}
	if opts, err := parseContinuousOptions(append([]string{"status", "--postgres-trust-remote-durability"}, pg...)); err != nil || !opts.target.trustRemoteDurability {
		t.Errorf("status with trust flag: %v", err)
	}
	for _, cmd := range [][]string{{"status"}, {"conversations"}, {"check-state"}, {"recover", "--dry-run"}, {"export", "id"}, {"import", "f"}, {"retain", "--dedup", "1h"}, {"migrate-runs"}} {
		if _, err := parseContinuousOptions(append(cmd, pg...)); err != nil {
			t.Errorf("%v: %v", cmd, err)
		}
	}
	for _, cmd := range [][]string{{"verify"}, {"backup", "f"}, {"restore", "f"}, {"format"}, {"migrate", "d"}} {
		if _, err := parseContinuousOptions(append(cmd, pg...)); err == nil || !strings.Contains(err.Error(), "journal-only") {
			t.Errorf("%v: %v", cmd, err)
		}
	}
	if _, err := parseContinuousOptions([]string{"verify-backup", "a", "--store-name", "main"}); err == nil {
		t.Error("verify-backup accepted postgres flags")
	}
	// Local behavior is unchanged: --backend journal stays import-only.
	if _, err := parseContinuousOptions([]string{"status", "--store", "x", "--backend", "sqlite"}); err == nil {
		t.Error("local --backend accepted on an existing-store command")
	}
	if _, err := parseContinuousOptions([]string{"status", "--store", "x", "--store-name", "main"}); err == nil {
		t.Error("--store-name accepted for a local store")
	}
}

func TestContinuousPostgresUnsupportedCommandSkipsCredentials(t *testing.T) {
	t.Setenv(continuousPostgresDSNEnv, "")
	err := runContinuous(context.Background(), []string{"backup", "f", "--backend", "postgres", "--store-name", "main", "--postgres-dsn-file", "/nonexistent"}, os.Stdout)
	if err == nil || !strings.Contains(err.Error(), "journal-only") {
		t.Fatalf("unsupported command reached credentials: %v", err)
	}
}

func TestPostgresDSNSources(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "dsn")
	secret := "postgres://u:hunter2@127.0.0.1/db?sslmode=disable"
	writePrivate(t, file, []byte(secret+"\r\n"))
	t.Setenv(continuousPostgresDSNEnv, "postgres://env@127.0.0.1/db?sslmode=disable")
	got, err := loadPostgresDSN(file)
	if err != nil || got != secret {
		t.Fatalf("file precedence: %q %v", got, err)
	}
	if got, err := loadPostgresDSN(""); err != nil || !strings.Contains(got, "env@") {
		t.Fatalf("environment fallback: %q %v", got, err)
	}
	// A selected unreadable file never falls back to the environment.
	if _, err := loadPostgresDSN(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file fell back")
	}
	t.Setenv(continuousPostgresDSNEnv, "")
	if _, err := loadPostgresDSN(""); err == nil {
		t.Fatal("no source accepted")
	}
	bad := map[string][]byte{
		"two-lines": []byte("host=127.0.0.1\nuser=u\n"),
		"nul":       []byte("host=127.0.0.1\x00"),
		"empty":     []byte("\n"),
		"oversized": []byte(strings.Repeat("a", 64<<10+1)),
	}
	for name, content := range bad {
		p := filepath.Join(dir, name)
		writePrivate(t, p, content)
		_, err := readPostgresDSNFile(p)
		if err == nil {
			t.Errorf("%s accepted", name)
		} else if strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("%s: error leaks content: %v", name, err)
		}
	}
	if runtime.GOOS != "windows" {
		open := filepath.Join(dir, "open")
		os.WriteFile(open, []byte(secret), 0o644)
		if _, err := readPostgresDSNFile(open); err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("group-readable file: %v", err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(file, link); err != nil {
			t.Fatal(err)
		}
		if _, err := readPostgresDSNFile(link); err == nil {
			t.Fatal("symlink accepted")
		}
		if _, err := readPostgresDSNFile(dir); err == nil {
			t.Fatal("directory accepted")
		}
	}
}

func TestHostStateLockAndIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	a, err := acquireHostState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireHostState(dir); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("second host: %v", err)
	}
	id := "00000000-0000-4000-8000-000000000001"
	if err := a.bind(backendPostgres, id); err != nil {
		t.Fatal(err)
	}
	if err := a.bind(backendPostgres, id); err != nil {
		t.Fatalf("rebind same identity: %v", err)
	}
	if err := a.bind(backendPostgres, "00000000-0000-4000-8000-000000000002"); err == nil {
		t.Fatal("conflicting identity accepted")
	}
	data, _ := os.ReadFile(filepath.Join(dir, stateIdentityFile))
	if strings.TrimSpace(string(data)) != `{"version":1,"backend":"postgres","store_uuid":"`+id+`"}` {
		t.Fatalf("manifest: %s", data)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dir, stateIdentityFile))
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("manifest mode %o", info.Mode().Perm())
		}
	}
	a.close()
	b, err := acquireHostState(dir)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	b.close()
	local := t.TempDir()
	os.WriteFile(filepath.Join(local, "boundary"), nil, 0o600)
	if _, err := acquireHostState(local); err == nil {
		t.Fatal("state dir holding a journal accepted")
	}
	if runtime.GOOS != "windows" {
		target := t.TempDir()
		linked := filepath.Join(t.TempDir(), "s")
		os.Mkdir(linked, 0o700)
		os.Symlink(filepath.Join(target, "x"), filepath.Join(linked, stateLockFile))
		if _, err := acquireHostState(linked); err == nil {
			t.Fatal("symlinked lock accepted")
		}
	}
}

// writePrivate writes a file that passes the platform's private-file check:
// mode 0600 on Unix, an owner and SYSTEM only access list on Windows.
func writePrivate(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := createPrivateExclusive(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Windows: a file with the default inherited access list is refused, one
// created with the owner-only descriptor is accepted. On Unix the same
// helper produces mode 0600.
func TestPrivateFileCheckRoundTrip(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	writePrivate(t, good, []byte("host=127.0.0.1"))
	f, err := openPrivateFile(good)
	if err != nil {
		t.Fatalf("private file refused: %v", err)
	}
	f.Close()
	inherited := filepath.Join(dir, "inherited")
	if err := os.WriteFile(inherited, []byte("host=127.0.0.1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if f, err := openPrivateFile(inherited); err == nil {
		f.Close()
		t.Fatal("file readable by others accepted")
	}
}

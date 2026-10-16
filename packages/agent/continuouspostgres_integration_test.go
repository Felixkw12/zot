package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/postgres"
)

// postgresTestDSN returns the dedicated disposable fixture, written to a
// private DSN file. Only ZOT_TEST_POSTGRES_DSN is consulted.
func postgresTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ZOT_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("ZOT_TEST_POSTGRES_REQUIRED") != "" {
			t.Fatal("ZOT_TEST_POSTGRES_REQUIRED is set but ZOT_TEST_POSTGRES_DSN is empty")
		}
		t.Skip("ZOT_TEST_POSTGRES_DSN not set, skipping PostgreSQL integration test")
	}
	file := filepath.Join(t.TempDir(), "dsn")
	writePrivate(t, file, []byte(dsn+"\n"))
	t.Setenv(continuousPostgresDSNEnv, "")
	return file
}

// The CLI runs a prompt against a PostgreSQL store, replays the request ID
// without new work, and administers it with existing-only commands.
func TestContinuousPostgresCLI(t *testing.T) {
	dsnFile := postgresTestDSN(t)
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "synthetic-key")
	ctx := context.Background()
	name := "cli-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	pg := []string{"--backend", "postgres", "--store-name", name, "--postgres-dsn-file", dsnFile}

	// Existing-only commands never create authority.
	var out bytes.Buffer
	if err := runContinuous(ctx, append([]string{"status"}, pg...), &out); !errors.Is(err, postgres.ErrStoreNotFound) {
		t.Fatalf("status on missing store: %v", err)
	}
	if err := runContinuousRun(ctx, append([]string{"--resume", "--provider", "openai", "--model", "gpt-4o-mini", "--no-ext", "--no-skills"}, pg...), &out); !errors.Is(err, postgres.ErrStoreNotFound) {
		t.Fatalf("resume on missing store: %v", err)
	}

	server := &fakeChatServer{script: []string{textChunk("hello from postgres")}}
	srv := httptest.NewServer(http.HandlerFunc(server.handler))
	defer srv.Close()
	host := []string{"--request-id", "req-1", "--provider", "openai", "--model", "gpt-4o-mini", "--base-url", srv.URL, "--no-ext", "--no-skills"}
	out.Reset()
	if err := runContinuousRun(ctx, append(append([]string{"say hello"}, pg...), host...), &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "hello from postgres") {
		t.Fatalf("answer: %q", out.String())
	}
	out.Reset()
	server.script = nil
	if err := runContinuousRun(ctx, append(append([]string{"say hello", "--json"}, pg...), host...), &out); err != nil {
		t.Fatalf("replay: %v\n%s", err, out.String())
	}
	if len(server.requests) != 1 || !strings.Contains(out.String(), `"state":"answered"`) {
		t.Fatalf("request ID replay executed work: %d %s", len(server.requests), out.String())
	}
	for _, cmd := range [][]string{{"status"}, {"check-state"}, {"conversations"}, {"outbox"}, {"recover", "--dry-run"}} {
		out.Reset()
		if err := runContinuous(ctx, append(cmd, pg...), &out); err != nil {
			t.Fatalf("%v: %v", cmd, err)
		}
		if !json.Valid(bytes.TrimSpace(out.Bytes())) {
			t.Fatalf("%v output: %s", cmd, out.String())
		}
	}
	// A held writer makes direct commands report the lock.
	dsn, _ := loadPostgresDSN(dsnFile)
	held, err := postgres.Open(ctx, dsn, postgres.Options{StoreName: name})
	if err != nil {
		t.Fatal(err)
	}
	if err := runContinuous(ctx, append([]string{"status"}, pg...), &out); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("status while held: %v", err)
	}
	held.Close()
	// Diagnostics never include the DSN.
	bad := filepath.Join(t.TempDir(), "bad")
	writePrivate(t, bad, []byte("postgres://u:hunter2@10.0.0.9/db?sslmode=disable"))
	err = runContinuous(ctx, []string{"status", "--backend", "postgres", "--store-name", name, "--postgres-dsn-file", bad}, &out)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("remote plaintext: %v", err)
	}
}

// serve on PostgreSQL: endpoints live in the locked state directory, bound
// to the store identity; an idle attached client disconnecting leaves the
// host healthy; losing the writer session stops serve with that cause and
// removes the endpoint.
func TestContinuousPostgresServe(t *testing.T) {
	dsnFile := postgresTestDSN(t)
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "synthetic-key")
	server := &fakeChatServer{script: []string{textChunk("hello from postgres host"), textChunk("second answer")}}
	srv := httptest.NewServer(http.HandlerFunc(server.handler))
	defer srv.Close()
	name := "serve-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	// Short path: macOS limits Unix socket paths to 104 bytes.
	stateDir, err := os.MkdirTemp("", "zp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateDir)
	sock := filepath.Join(stateDir, "host.sock")
	args := []string{"--backend", "postgres", "--store-name", name, "--postgres-dsn-file", dsnFile, "--state-dir", stateDir,
		"--provider", "openai", "--model", "gpt-4o-mini", "--base-url", srv.URL, "--no-ext", "--no-skills"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- runContinuousServe(ctx, args, os.Stderr) }()
	waitEndpoint(t, ctx, sock, served)

	// A second host on the same state directory is refused before it
	// touches the database or the socket.
	if err := runContinuousServe(ctx, args, os.Stderr); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("second host: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, stateIdentityFile))
	if err != nil || !strings.Contains(string(data), `"backend":"postgres"`) {
		t.Fatalf("identity not bound: %s %v", data, err)
	}
	var out bytes.Buffer
	if err := runContinuousAttach(ctx, []string{"say hello", "--socket", sock, "--workspace", "ws"}, &out); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if strings.TrimSpace(out.String()) != "hello from postgres host" {
		t.Fatalf("answer: %q", out.String())
	}
	// An idle client that disconnects mid-watch does not cancel host work.
	followCtx, stopFollow := context.WithCancel(ctx)
	followed := make(chan error, 1)
	go func() {
		followed <- runContinuousAttach(followCtx, []string{"--follow", "--socket", sock, "--workspace", "ws"}, io.Discard)
	}()
	stopFollow()
	<-followed
	out.Reset()
	if err := runContinuousAttach(ctx, []string{"again", "--socket", sock, "--workspace", "ws"}, &out); err != nil || strings.TrimSpace(out.String()) != "second answer" {
		t.Fatalf("host after client disconnect: %q %v", out.String(), err)
	}

	// Lose the writer session. serve must stop with the store failure,
	// not report a clean shutdown, and must not keep the endpoint.
	dsn, _ := loadPostgresDSN(dsnFile)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = 'zot-continuous' AND pid <> pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if err == nil || !strings.Contains(err.Error(), "continuous postgres") {
			t.Fatalf("serve did not report the store failure: %v", err)
		}
		t.Logf("serve stopped with: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("serve kept running after losing storage")
	}
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(sock); err == nil {
			t.Fatal("endpoint left behind after store failure")
		}
	}
	// The state directory is released and reusable for the same store.
	state, err := acquireHostState(stateDir)
	if err != nil {
		t.Fatalf("state lock not released: %v", err)
	}
	state.close()
}

func waitEndpoint(t *testing.T, ctx context.Context, sock string, served <-chan error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		probe, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		conn, err := continuous.DialLocal(probe, sock)
		cancel()
		if err == nil {
			conn.Close()
			return
		}
		select {
		case err := <-served:
			t.Fatalf("serve exited early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("endpoint not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

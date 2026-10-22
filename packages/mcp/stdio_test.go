package mcp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	protocol "github.com/mark3labs/mcp-go/mcp"
)

func TestStdioOutputMarksOnlyUnexpectedProtocolClosure(t *testing.T) {
	for _, closing := range []bool{false, true} {
		s := &server{}
		s.closing.Store(closing)
		output := &stdioOutput{Reader: bytes.NewReader(nil), server: s}
		_, err := output.Read(make([]byte, 1))
		if err != io.EOF || s.lost.Load() == closing {
			t.Fatalf("closing=%v: err=%v lost=%v", closing, err, s.lost.Load())
		}
	}
}

func TestStdioCloseReapsProcessOnce(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: ServerConfig{
		Command: binary,
		Args:    []string{"-test.run=^TestMCPStdioHelper$"},
		Env:     map[string]string{"ZOT_MCP_HELPER": "1"},
		CWD:     t.TempDir(),
	}}
	c, err := startStdio(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	s.closing.Store(true)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { c.Close() })
	}
	wg.Wait()
	if s.process.ProcessState == nil {
		t.Fatal("server process was not reaped")
	}
	if s.lost.Load() {
		t.Fatal("intentional shutdown was reported as a lost connection")
	}
}

// A server that exits once stdin closes is never signalled, so it can
// clean up. One that ignores stdin and the interrupt is still killed.
func TestStdioCloseLetsServersExitGracefullyThenKills(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	previous := shutdownGrace.Load()
	shutdownGrace.Store(int64(500 * time.Millisecond))
	t.Cleanup(func() { shutdownGrace.Store(previous) })
	for _, stubborn := range []bool{false, true} {
		marker := filepath.Join(t.TempDir(), "exit")
		// A race-built helper otherwise sleeps one second at exit.
		env := map[string]string{"ZOT_MCP_HELPER": "1", "ZOT_MCP_EXIT_MARKER": marker, "GORACE": "atexit_sleep_ms=0"}
		if stubborn {
			env["ZOT_MCP_STUBBORN"] = "1"
		}
		s := &server{cfg: ServerConfig{Command: binary, Args: []string{"-test.run=^TestMCPStdioHelper$"}, Env: env, CWD: t.TempDir()}}
		c, err := startStdio(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		// Wait until the server answers, so the grace period measures its
		// shutdown and not its startup.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if _, err := c.Initialize(ctx, protocol.InitializeRequest{}); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		s.closing.Store(true)
		c.Close()
		state := s.process.ProcessState
		if state == nil {
			t.Fatalf("stubborn=%v: server process was not reaped", stubborn)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("stubborn=%v: server did not observe the closed stdin: %v", stubborn, err)
		}
		// Exit status 0 proves the server was not signalled.
		if !stubborn && !state.Success() {
			t.Fatalf("cooperative server was not left to exit on its own: %v", state)
		}
		if stubborn && state.Success() {
			t.Fatalf("stubborn server was not killed: %v", state)
		}
	}
}

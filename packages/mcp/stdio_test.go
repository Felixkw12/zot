package mcp

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
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

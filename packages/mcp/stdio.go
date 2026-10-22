package mcp

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
)

// stdioOutput observes the protocol stream, not stderr. A server may close
// its diagnostic stream without exiting or dropping its MCP connection.
type stdioOutput struct {
	io.Reader
	server *server
}

func (r *stdioOutput) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && !r.server.closing.Load() {
		r.server.lost.Store(true)
	}
	return n, err
}

// stdioTransport owns the process because NewIO only owns the protocol
// streams. Close follows the MCP shutdown order: close stdin so the server
// can exit on its own, then interrupt it, and finally kill the process
// group. The direct child is reaped once.
type stdioTransport struct {
	*transport.Stdio
	cmd    *exec.Cmd
	stdout io.ReadCloser
	once   sync.Once
	err    error
}

// shutdownGrace is how long a server gets after stdin closes, and again
// after an interrupt, before it is killed. Tests shorten it.
var shutdownGrace atomic.Int64

func init() { shutdownGrace.Store(int64(2 * time.Second)) }

func (t *stdioTransport) Close() error {
	t.once.Do(func() {
		// Stdio.Close closes stdin, which is the first shutdown signal.
		t.err = t.Stdio.Close()
		waited := make(chan error, 1)
		go func() { waited <- t.cmd.Wait() }()
		grace := time.Duration(shutdownGrace.Load())
		var err error
		select {
		case err = <-waited:
		case <-time.After(grace):
			interruptProcess(t.cmd)
			select {
			case err = <-waited:
			case <-time.After(grace):
				terminateProcess(t.cmd)
				err = <-waited
			}
		}
		// A wrapper such as npx may exit before its children. Sweep the
		// process group so nothing outlives the connection.
		terminateProcess(t.cmd)
		t.stdout.Close()
		if t.err == nil {
			t.err = err
		}
	})
	return t.err
}

func startStdio(s *server, logger *slog.Logger) (*client.Client, error) {
	cfg := s.cfg
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = os.Environ()
	for key, value := range cfg.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Dir = cfg.CWD
	stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	defer stderr.Close()
	cmd.Stderr = stderr
	configureProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	s.process = cmd
	stdio := transport.NewIO(&stdioOutput{Reader: stdout, server: s}, stdin, nil)
	transport.WithCommandLogger(logger)(stdio)
	t := &stdioTransport{Stdio: stdio, cmd: cmd, stdout: stdout}
	if err := t.Start(context.Background()); err != nil {
		t.Close()
		return nil, err
	}
	return client.NewClient(t), nil
}

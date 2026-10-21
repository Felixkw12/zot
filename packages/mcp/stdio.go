package mcp

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"

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
// streams. Close kills the process group and reaps the direct child once.
type stdioTransport struct {
	*transport.Stdio
	cmd    *exec.Cmd
	stdout io.ReadCloser
	once   sync.Once
	err    error
}

func (t *stdioTransport) Close() error {
	t.once.Do(func() {
		terminateProcess(t.cmd)
		t.err = t.Stdio.Close()
		t.stdout.Close()
		if err := t.cmd.Wait(); t.err == nil {
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

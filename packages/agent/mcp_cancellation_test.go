package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/core"
)

type blockingMCPRefresh struct {
	changingMCPSource
	entered chan context.Context
	release chan struct{}
}

func (s *blockingMCPRefresh) RefreshStale(ctx context.Context) bool {
	s.entered <- ctx
	select {
	case <-ctx.Done():
	case <-s.release:
	}
	return false
}

func TestMCPRefreshUsesPromptCancellation(t *testing.T) {
	mgr := extensions.New(t.TempDir(), t.TempDir(), "test", "", "", nonInteractiveExtHooks{})
	t.Cleanup(func() { mgr.Stop(time.Millisecond) })
	source := &blockingMCPRefresh{entered: make(chan context.Context, 1), release: make(chan struct{})}
	mgr.AddBuiltin("mcp", source)
	ag := core.NewAgent(nil, "test", "", nil)
	wireNonInteractiveAgentExtLifecycle(context.Background(), ag, mgr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ag.Prompt(ctx, "hello", nil, nil) }()
	select {
	case refreshCtx := <-source.entered:
		cancel()
		if !errors.Is(refreshCtx.Err(), context.Canceled) {
			t.Error("MCP refresh did not receive the prompt's cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Error("MCP refresh did not start")
	}
	// Also release a broken implementation that used the host context.
	close(source.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("prompt error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled prompt did not finish")
	}
}

package continuous

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestNestedDurableCompletedChildIsNotRepeated(t *testing.T) {
	inner := &effectTool{name: "inner", replay: core.ReplayNever}
	entered := make(chan struct{}, 1)
	block := make(chan struct{})
	outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer", replay: core.ReplaySafe}}
	outer.execute = func(ctx context.Context) (core.ToolResult, error) {
		result := core.ToolRuntimeFromContext(ctx).Call(ctx, "inner", json.RawMessage(`{}`))
		entered <- struct{}{}
		select {
		case <-block:
			return result, nil
		case <-ctx.Done():
			return core.ToolResult{}, ctx.Err()
		}
	}
	h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("parent", "outer", `{}`)}}, {text: "done"}}, outer, inner)
	defer h.r.Close()
	c := h.root(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := h.r.Submit(ctx, c.ID, "actor", "", "hello"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := h.svc.Step(ctx, c.ID)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("parent did not start")
	}
	cancel()
	<-done
	close(block)
	run, _, err := h.svc.Step(context.Background(), c.ID)
	if err != nil || run.Outcome != "completed" || inner.calls.Load() != 1 {
		t.Fatalf("nested replay: run=%+v err=%v executions=%d", run, err, inner.calls.Load())
	}
	tasks, err := h.r.Tasks(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	tools := 0
	for _, task := range tasks {
		if task.Kind == TaskKindTool {
			tools++
		}
	}
	if tools != 2 {
		t.Fatalf("missing durable child: tools=%d", tools)
	}
	// Exactly one model-visible result, for the parent only.
	results := 0
	for _, entry := range h.entries(t, c.ID) {
		if entry.Type == entryToolResult {
			results++
		}
	}
	if results != 1 {
		t.Fatalf("nested call wrote transcript: results=%d", results)
	}
}

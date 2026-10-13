package continuous

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestNestedAbortSettlesBottomUpAndPreservesChildResult(t *testing.T) {
	leaf := &effectTool{name: "leaf", block: make(chan struct{}), started: make(chan struct{}, 1)}
	middle := &nestedSecurityTool{effectTool: &effectTool{name: "middle", replay: core.ReplaySafe}}
	outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer", replay: core.ReplaySafe}}
	middle.execute = func(ctx context.Context) (core.ToolResult, error) {
		return core.ToolRuntimeFromContext(ctx).Call(ctx, "leaf", json.RawMessage(`{}`)), nil
	}
	outer.execute = func(ctx context.Context) (core.ToolResult, error) {
		return core.ToolRuntimeFromContext(ctx).Call(ctx, "middle", json.RawMessage(`{}`)), nil
	}
	h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("parent", "outer", `{}`)}}}, outer, middle, leaf)
	defer h.r.Close()
	c := h.root(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.r.Submit(ctx, c.ID, "actor", "", "go"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := h.svc.Step(ctx, c.ID); done <- err }()
	select {
	case <-leaf.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := h.r.Abort(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	close(leaf.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	tasks, err := h.r.Tasks(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Task{}
	for _, task := range tasks {
		if in, _, err := decodeTool(task); err == nil {
			byName[in.Name] = task
		}
	}
	for _, name := range []string{"outer", "middle", "leaf"} {
		if byName[name].State != "terminal" {
			t.Fatalf("%s is live: %+v", name, byName[name])
		}
	}
	if byName["leaf"].Revision >= byName["middle"].Revision || byName["middle"].Revision >= byName["outer"].Revision {
		t.Fatal("parent settled before descendants")
	}
	var outcome toolOutcome
	if err := json.Unmarshal(byName["leaf"].Result, &outcome); err != nil || outcome.Nested == nil {
		t.Fatalf("leaf result: %+v %v", outcome, err)
	}
	result, err := outcome.Nested.result()
	if err != nil || result.IsError || outcome.Status != "completed" {
		t.Fatalf("known child result overwritten by abort: %+v %v", result, err)
	}
	checkValid(t, h.r)
}

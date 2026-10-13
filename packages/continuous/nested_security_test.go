package continuous

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type nestedSecurityTool struct {
	*effectTool
	execute func(context.Context) (core.ToolResult, error)
}

func (t *nestedSecurityTool) Execute(ctx context.Context, _ json.RawMessage, _ func(string)) (core.ToolResult, error) {
	return t.execute(ctx)
}

func TestNestedSecurityGuard(t *testing.T) {
	inner := &effectTool{name: "inner"}
	var received core.ToolResult
	outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer"}}
	outer.execute = func(ctx context.Context) (core.ToolResult, error) {
		received = core.ToolRuntimeFromContext(ctx).Call(ctx, "inner", json.RawMessage(`{}`))
		return received, nil
	}
	h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("c1", "outer", `{}`)}}, {text: "done"}}, outer, inner)
	defer h.r.Close()
	h.guard = func(tc provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return tc.Name != "inner", "nested denied", nil
	}
	c := h.root(t)
	if _, err := h.r.Submit(context.Background(), c.ID, "actor", "", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.svc.Step(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	if inner.calls.Load() != 0 || !received.IsError || received.Status != "blocked" {
		t.Fatalf("nested guard bypass: calls=%d result=%+v", inner.calls.Load(), received)
	}
}

func TestNestedSecurityIdentityAndReplayKey(t *testing.T) {
	var mu sync.Mutex
	var parents, children []ToolCallIdentity
	var parentKeys, childKeys []string
	started := make(chan struct{}, 1)
	block := make(chan struct{})
	inner := &nestedSecurityTool{effectTool: &effectTool{name: "inner"}}
	inner.execute = func(ctx context.Context) (core.ToolResult, error) {
		id, ok := ToolCallFromContext(ctx)
		if !ok {
			t.Error("missing child identity")
		}
		mu.Lock()
		children = append(children, id)
		childKeys = append(childKeys, core.ToolOperationKey(ctx))
		mu.Unlock()
		return core.ToolResult{}, nil
	}
	outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer", replay: core.ReplaySafe}}
	outer.execute = func(ctx context.Context) (core.ToolResult, error) {
		id, _ := ToolCallFromContext(ctx)
		mu.Lock()
		parents = append(parents, id)
		parentKeys = append(parentKeys, core.ToolOperationKey(ctx))
		mu.Unlock()
		result := core.ToolRuntimeFromContext(ctx).Call(ctx, "inner", json.RawMessage(`{}`))
		started <- struct{}{}
		select {
		case <-block:
			return result, nil
		case <-ctx.Done():
			return core.ToolResult{}, ctx.Err()
		}
	}
	h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("c1", "outer", `{}`)}}, {text: "done"}}, outer, inner)
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
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("parent never started")
	}
	cancel()
	<-done
	close(block)
	run, _, err := h.svc.Step(context.Background(), c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("replay: %+v %v", run, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(children) != 2 || len(parents) != 2 {
		t.Fatalf("calls: parents=%v children=%v", parents, children)
	}
	t.Run("identity", func(t *testing.T) {
		if children[0] == parents[0] || children[0].CallID != "c1/1" || children[0].ConversationID != c.ID || children[0] != children[1] {
			t.Fatalf("child identity: parents=%v children=%v", parents, children)
		}
	})
	t.Run("operation_key", func(t *testing.T) {
		if childKeys[0] == "" || childKeys[0] == parentKeys[0] || childKeys[0] != childKeys[1] || childKeys[0] != children[0].OperationKey() {
			t.Fatalf("operation keys: parents=%v children=%v", parentKeys, childKeys)
		}
	})
}

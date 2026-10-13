package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// A nested approval parks like a top-level one: Step returns
// ErrAwaitingApproval instead of blocking, nothing executes, a second Step
// does not ask twice, and the decision resumes the same child exactly once.
func TestNestedApprovalStepReturnsAwaiting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	inner := &effectTool{name: "effect"}
	var outerRuns int
	outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer", replay: core.ReplaySafe}}
	outer.execute = func(ctx context.Context) (core.ToolResult, error) {
		outerRuns++
		return core.ToolRuntimeFromContext(ctx).Call(ctx, "effect", json.RawMessage(`{}`)), nil
	}
	h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("parent", "outer", `{}`)}}, {text: "done"}}, outer, inner)
	h.svc.opts.Approver = requireApproval
	defer h.r.Close()
	c := h.root(t)
	if _, err := h.r.Submit(ctx, c.ID, "actor", "", "go"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, err := h.svc.Step(ctx, c.ID); !errors.Is(err, ErrAwaitingApproval) {
			t.Fatalf("step: %v", err)
		}
	}
	pending, err := h.r.PendingApprovals(ctx, c.ID)
	if err != nil || len(pending) != 1 || pending[0].ParentTool != "outer" || inner.calls.Load() != 0 {
		t.Fatalf("pending: %+v %v executions=%d", pending, err, inner.calls.Load())
	}
	checkValid(t, h.r)
	if _, err := h.r.Decide(ctx, pending[0].ID, "human", true, "call", ""); err != nil {
		t.Fatal(err)
	}
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" || inner.calls.Load() != 1 {
		t.Fatalf("run: %+v %v executions=%d", run, err, inner.calls.Load())
	}
	tasks, _ := h.r.Tasks(ctx, c.ID)
	children := 0
	for _, task := range tasks {
		if in, _, err := decodeTool(task); err == nil && in.ParentTask != "" {
			children++
		}
	}
	if children != 1 || outerRuns < 2 {
		t.Fatalf("children=%d outerRuns=%d", children, outerRuns)
	}
	checkValid(t, h.r)
}

// A parent that cannot replay keeps waiting: releasing it would turn the
// approval wait into an interrupted call.
func TestNestedApprovalNeverParentKeepsWaiting(t *testing.T) {
	inner := &effectTool{name: "effect"}
	outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer", replay: core.ReplayNever}}
	outer.execute = func(ctx context.Context) (core.ToolResult, error) {
		return core.ToolRuntimeFromContext(ctx).Call(ctx, "effect", json.RawMessage(`{}`)), nil
	}
	h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("parent", "outer", `{}`)}}, {text: "done"}}, outer, inner)
	h.svc.opts.Approver = requireApproval
	defer h.r.Close()
	c := h.root(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := h.r.Submit(ctx, c.ID, "actor", "", "go"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := h.svc.Step(ctx, c.ID); done <- err }()
	var id string
	waitNestedState(t, h.r, func(snap storage.Snapshot) bool {
		pending, _ := pendingApprovals(snap, c.ID)
		if len(pending) == 1 {
			id = pending[0].ID
		}
		return id != ""
	})
	select {
	case err := <-done:
		t.Fatalf("never parent released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := h.r.Decide(context.Background(), id, "human", true, "call", ""); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || inner.calls.Load() != 1 {
		t.Fatalf("step: %v executions=%d", err, inner.calls.Load())
	}
}

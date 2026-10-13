package continuous

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func waitNestedState(t *testing.T, r *Runtime, visit func(storage.Snapshot) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if visit(snap) {
			return
		}
		if err := r.store.Wait(ctx, snap.Revision()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNestedExplicitKeysReorderAndMismatch(t *testing.T) {
	inner := &effectTool{name: "inner", replay: core.ReplayNever}
	outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer", replay: core.ReplaySafe}}
	entered := make(chan struct{}, 1)
	replay := false
	outer.execute = func(ctx context.Context) (core.ToolResult, error) {
		keys := []string{"a", "b"}
		if replay {
			keys = []string{"b", "a"}
		}
		rt := core.ToolRuntimeFromContext(ctx)
		for _, key := range keys {
			result := rt.Call(core.WithNestedToolKey(ctx, key), "inner", json.RawMessage(`{"n":1}`))
			if result.IsError {
				t.Errorf("key %s: %+v", key, result)
			}
		}
		if !replay {
			entered <- struct{}{}
			<-ctx.Done()
			return core.ToolResult{}, ctx.Err()
		}
		mismatch := rt.Call(core.WithNestedToolKey(ctx, "a"), "inner", json.RawMessage(`{"n":2}`))
		if !mismatch.IsError || mismatch.Status != "blocked" {
			t.Errorf("mismatch: %+v", mismatch)
		}
		return core.ToolResult{}, nil
	}
	h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("parent", "outer", `{}`)}}, {text: "done"}}, outer, inner)
	defer h.r.Close()
	c := h.root(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.r.Submit(ctx, c.ID, "actor", "", "go")
	done := make(chan error, 1)
	go func() { _, _, err := h.svc.Step(ctx, c.ID); done <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("not started")
	}
	cancel()
	<-done
	replay = true
	if _, _, err := h.svc.Step(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	if inner.calls.Load() != 2 {
		t.Fatalf("effects repeated: %d", inner.calls.Load())
	}
	if report, err := h.r.CheckIntegrity(context.Background()); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func TestNestedApprovalSurvivesRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	inner := &effectTool{name: "effect"}
	outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer", replay: core.ReplaySafe}}
	outer.execute = func(ctx context.Context) (core.ToolResult, error) {
		return core.ToolRuntimeFromContext(ctx).Call(ctx, "effect", json.RawMessage(`{}`)), nil
	}
	open := func(steps []scriptStep) *harness {
		store, err := journal.Open(context.Background(), dir, journal.Options{Durability: storage.Process})
		if err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, store, steps, outer, inner)
		h.svc.opts.Approver = requireApproval
		return h
	}
	h := open([]scriptStep{{calls: []provider.ToolCallBlock{call("parent", "outer", `{}`)}}})
	c := h.root(t)
	h.r.Submit(context.Background(), c.ID, "actor", "", "go")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := h.svc.Step(ctx, c.ID); done <- err }()
	var approval Approval
	waitNestedState(t, h.r, func(snap storage.Snapshot) bool {
		rows, _ := snap.Page("approval/", "", 1)
		if len(rows) == 0 {
			return false
		}
		json.Unmarshal(rows[0].Value, &approval)
		return approval.State == approvalPending
	})
	if inner.calls.Load() != 0 || approval.ParentTool != "outer" {
		t.Fatalf("approval: %+v", approval)
	}
	plan, err := h.r.RecoveryPreview(context.Background())
	if err != nil || len(plan.Actions) != 1 || len(plan.Actions[0].Nested) != 1 || plan.Actions[0].Nested[0].Action != "approve" {
		t.Fatalf("preview: %+v %v", plan, err)
	}
	cancel()
	<-done
	h.r.Close()
	h = open([]scriptStep{{text: "done"}})
	defer h.r.Close()
	pending, err := h.r.PendingApprovals(context.Background(), c.ID)
	if err != nil || len(pending) != 1 || pending[0].ID != approval.ID {
		t.Fatalf("lost approval: %+v %v", pending, err)
	}
	if _, err := h.r.Decide(context.Background(), approval.ID, "human", true, "call", ""); err != nil {
		t.Fatal(err)
	}
	run, _, err := h.svc.Step(context.Background(), c.ID)
	if err != nil || run.Outcome != "completed" || inner.calls.Load() != 1 {
		t.Fatalf("run: %+v %v executions=%d", run, err, inner.calls.Load())
	}
}

func TestNestedNeverParentAbortsLiveChild(t *testing.T) {
	inner := &receiverTool{policy: core.ReplaySafe, block: make(chan struct{}), started: make(chan struct{}, 1)}
	outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer", replay: core.ReplayNever}}
	outer.execute = func(ctx context.Context) (core.ToolResult, error) {
		return core.ToolRuntimeFromContext(ctx).Call(ctx, "send", json.RawMessage(`{}`)), nil
	}
	h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("parent", "outer", `{}`)}}, {text: "done"}}, outer, inner)
	defer h.r.Close()
	c := h.root(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.r.Submit(ctx, c.ID, "actor", "", "go")
	done := make(chan error, 1)
	go func() { _, _, err := h.svc.Step(ctx, c.ID); done <- err }()
	select {
	case <-inner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("child not started")
	}
	cancel()
	<-done
	run, _, err := h.svc.Step(context.Background(), c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("recovery: %+v %v", run, err)
	}
	tasks, _ := h.r.Tasks(context.Background(), c.ID)
	found := false
	for _, task := range tasks {
		if in, _, err := decodeTool(task); err == nil && in.ParentTask != "" {
			found = true
			if task.State != "terminal" || task.Outcome != "aborted" || !strings.Contains(task.Error, "effect unknown") {
				t.Fatalf("child: %+v", task)
			}
		}
	}
	if !found || len(inner.keys) != 1 {
		t.Fatalf("child missing or repeated: %d", len(inner.keys))
	}
}

func TestNestedReconcileUsesChildKey(t *testing.T) {
	inner := &receiverTool{policy: core.ReplayReconcile, block: make(chan struct{}), started: make(chan struct{}, 1), outcome: core.ReconcileCompleted}
	outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer", replay: core.ReplaySafe}}
	var parentKey string
	outer.execute = func(ctx context.Context) (core.ToolResult, error) {
		parentKey = core.ToolOperationKey(ctx)
		result := core.ToolRuntimeFromContext(ctx).Call(ctx, "send", json.RawMessage(`{}`))
		if result.IsError {
			return result, nil
		}
		return result, nil
	}
	h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("parent", "outer", `{}`)}}, {text: "done"}}, outer, inner)
	defer h.r.Close()
	c := h.root(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.r.Submit(ctx, c.ID, "actor", "", "go")
	done := make(chan error, 1)
	go func() { _, _, err := h.svc.Step(ctx, c.ID); done <- err }()
	select {
	case <-inner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("child not started")
	}
	cancel()
	<-done
	run, _, err := h.svc.Step(context.Background(), c.ID)
	if err != nil || run.Outcome != "completed" || len(inner.keys) != 1 || inner.recCalls != 1 || inner.keys[0] == parentKey {
		t.Fatalf("reconcile: %+v %v keys=%v parent=%s calls=%d", run, err, inner.keys, parentKey, inner.recCalls)
	}
	entries := h.entries(t, c.ID)
	found := false
	for _, entry := range entries {
		if strings.Contains(string(entry.Message), "reconciled "+inner.keys[0]) {
			found = true
		}
	}
	if !found {
		t.Fatal("reconciliation did not receive child's operation key")
	}
}

func TestNestedBoundedResultAndProgressProjection(t *testing.T) {
	stored := boundedNestedResult(core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: strings.Repeat("ä", 400000)}}})
	data, err := json.Marshal(stored)
	if err != nil || len(data) > maxNestedResultBytes || !stored.Truncated {
		t.Fatalf("bound: %d %v", len(data), err)
	}
	result, err := stored.result()
	if err != nil || !strings.Contains(core.ToolResultText(result), "truncated") {
		t.Fatalf("result: %+v %v", result, err)
	}
	ev := projectToolProgress(ToolProgress{CallID: "parent/n/1", ParentCallID: "parent"}, "working")
	if ev.ID != "parent" || !strings.Contains(ev.Text, "parent/n/1") {
		t.Fatalf("progress: %+v", ev)
	}
}

package continuous

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestSchedulerWaitDoesNotLoseCommitAfterSelection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, _ := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	sched := NewTaskScheduler(r, TaskRegistry{})
	if _, _, err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.commit(ctx, snap, "test.wakeup", record("test/wakeup", true)); err != nil {
		t.Fatal(err)
	}
	if err := sched.waitProgress(ctx, time.Time{}); err != nil {
		t.Fatalf("lost commit between selection and waiting: %v", err)
	}
}

func TestNestedSubagentMakesProgressWithOneWorker(t *testing.T) {
	outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer", replay: core.ReplaySafe}}
	middle := &nestedSecurityTool{effectTool: &effectTool{name: "middle", replay: core.ReplaySafe}}
	outer.execute = func(ctx context.Context) (core.ToolResult, error) {
		return core.ToolRuntimeFromContext(ctx).Call(ctx, "middle", json.RawMessage(`{}`)), nil
	}
	middle.execute = func(ctx context.Context) (core.ToolResult, error) {
		return core.ToolRuntimeFromContext(ctx).Call(ctx, "subagent", json.RawMessage(`{"task":"count files"}`)), nil
	}
	h := newNativeHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("parent", "outer", `{}`)}}, {text: "three files"}, {text: "done"}}, outer, middle)
	h.tools["subagent"] = &SubagentTool{Runtime: h.r, Service: h.svc}
	defer h.r.Close()
	h.sched.Workers = 1
	c := h.nativeRoot(t)
	if _, err := h.r.Submit(context.Background(), c.ID, "actor", "", "go"); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	entries := h.entries(t, c.ID)
	found := false
	for _, entry := range entries {
		if entry.Type == entryToolResult && entry.Content == "three files" {
			found = true
		}
	}
	if !found {
		t.Fatalf("nested subagent failed: %+v", entries)
	}
	checkValid(t, h.r)
}

package continuous

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestNestedIntegrityRejectsCorruption(t *testing.T) {
	for _, kind := range []string{"orphan", "duplicate", "transcript", "depth", "background"} {
		t.Run(kind, func(t *testing.T) {
			inner := &effectTool{name: "inner"}
			outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer"}}
			outer.execute = func(ctx context.Context) (core.ToolResult, error) {
				return core.ToolRuntimeFromContext(ctx).Call(ctx, "inner", json.RawMessage(`{}`)), nil
			}
			h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("parent", "outer", `{}`)}}, {text: "done"}}, outer, inner)
			defer h.r.Close()
			c := h.root(t)
			ctx := context.Background()
			h.r.Submit(ctx, c.ID, "actor", "", "go")
			if _, _, err := h.svc.Step(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
			tasks, _ := h.r.Tasks(ctx, c.ID)
			var child Task
			var in toolInput
			for _, task := range tasks {
				if input, _, err := decodeTool(task); err == nil && input.ParentTask != "" {
					child, in = task, input
				}
			}
			if child.ID == "" {
				t.Fatal("no child")
			}
			snap, _ := h.r.store.Snapshot(ctx)
			var ops []storage.Operation
			switch kind {
			case "orphan":
				in.ParentTask = uuid.NewString()
			case "duplicate":
				child.ID = uuid.NewString()
			case "depth":
				in.Depth = 4
			case "background":
				child.Background = true
			case "transcript":
				conv, _ := conversation(snap, c.ID)
				conv.EntrySequence++
				conv.Revision = snap.Revision() + 1
				msg := provider.Message{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: in.CallID, Content: []provider.Content{provider.TextBlock{Text: "illegal"}}}}}
				entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, Type: entryToolResult, Revision: snap.Revision() + 1, Message: marshalMessage(msg)}
				ops = append(ops, record(entryKey(c.ID, conv.EntrySequence), entry), record("conversation/"+c.ID, conv))
			}
			if kind != "transcript" {
				child.Input, _ = json.Marshal(in)
				child.Revision = snap.Revision() + 1
				ops = append(ops, record(taskKey(child.ID), child), record(taskOwnerKey(child.Owner, child.ID), child.ID), record(taskConversationKey(c.ID, child.ID), child.ID))
			}
			if err := h.r.commit(ctx, snap, "test.corrupt", ops...); err != nil {
				t.Fatal(err)
			}
			if _, err := h.r.CheckIntegrity(ctx); err == nil {
				t.Fatal("corruption accepted")
			}
		})
	}
}

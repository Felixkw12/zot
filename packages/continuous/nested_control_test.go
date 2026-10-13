package continuous

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// controlTool returns every control signal a model-issued result may carry.
type controlTool struct{ *effectTool }

func (t controlTool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	t.calls.Add(1)
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "control"}}, Terminate: true, ActivateTools: []string{"hidden"}}, nil
}

// Control signals apply only to model-issued calls. A nested handoff,
// terminate, or tool activation is data for the caller and never resets the
// context, ends the run, or changes the tool set.
func TestNestedResultsCarryNoControl(t *testing.T) {
	ctx := context.Background()
	var handoff, control core.ToolResult
	outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer"}}
	outer.execute = func(ctx context.Context) (core.ToolResult, error) {
		rt := core.ToolRuntimeFromContext(ctx)
		handoff = rt.Call(ctx, "handoff", json.RawMessage(`{"note":"state","continue":"go on"}`))
		control = rt.Call(ctx, "control", json.RawMessage(`{}`))
		return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "outer done"}}}, nil
	}
	h := newHarness(t, newMemoryStore(), []scriptStep{
		{calls: []provider.ToolCallBlock{call("parent", "outer", `{}`)}},
		{text: "finished"},
	}, outer, HandoffTool{}, controlTool{&effectTool{name: "control"}})
	defer h.r.Close()
	c := h.root(t)
	if _, err := h.r.Submit(ctx, c.ID, "actor", "", "go"); err != nil {
		t.Fatal(err)
	}
	run, _, err := h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("run: %+v %v", run, err)
	}
	if handoff.IsError || core.ToolResultText(handoff) != "handing off to a fresh context" {
		t.Fatalf("nested handoff result: %+v", handoff)
	}
	if control.Terminate || len(control.ActivateTools) != 0 {
		t.Fatalf("nested control leaked to caller: %+v", control)
	}
	// The model continued after the round: no reset, no continuation input.
	if got := entryTypes(h.entries(t, c.ID)); got != "user assistant tool_result assistant" {
		t.Fatalf("entries: %s", got)
	}
	if cur, _ := h.r.Conversation(ctx, c.ID); cur.QueueSequence != 1 {
		t.Fatalf("nested handoff admitted a continuation: %d", cur.QueueSequence)
	}
	tasks, _ := h.r.Tasks(ctx, c.ID)
	for _, task := range tasks {
		if strings.Contains(string(task.Result), `"terminate"`) || strings.Contains(string(task.Result), `"activated"`) {
			t.Fatalf("control stored on task: %s", task.Result)
		}
	}
	checkValid(t, h.r)
}

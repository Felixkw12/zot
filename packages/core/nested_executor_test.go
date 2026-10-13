package core

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestNestedToolExecutorWrapsExecution(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		name := "execute"
		if blocked {
			name = "block"
		}
		t.Run(name, func(t *testing.T) {
			tool := &recordingTool{}
			ag := NewAgent(nil, "test", "", NewRegistry(orchestrationTool{name: "outer", next: "echo"}, tool))
			hooks, guards := 0, 0
			ag.BeforeToolExecute = func(provider.ToolCallBlock) (bool, string, json.RawMessage) {
				guards++
				return true, "", nil
			}
			ag.NestedToolExecutor = func(ctx context.Context, parent, call provider.ToolCallBlock, supplied Tool, execute func(context.Context, provider.ToolCallBlock) ToolResult) ToolResult {
				hooks++
				runtime := ToolRuntimeFromContext(ctx)
				if runtime == nil || runtime.Agent() != ag || parent.ID != "root" || call.ID != "root/1" || call.Name != "echo" || supplied != nil {
					t.Fatalf("nested execution context: parent=%+v call=%+v tool=%v", parent, call, supplied)
				}
				if blocked {
					return ToolResult{IsError: true, Status: "blocked"}
				}
				call.Arguments = json.RawMessage(`{"effective":true}`)
				return execute(ctx, call)
			}
			result := ag.CallTool(context.Background(), "root", "outer", json.RawMessage(`{}`), nil)
			if hooks != 1 || result.IsError != blocked || len(result.NestedCalls) != 1 || result.NestedCalls[0].Executed == blocked {
				t.Fatalf("nested lifecycle: hooks=%d result=%+v", hooks, result)
			}
			if blocked {
				if guards != 1 || tool.lastArgs != nil || result.NestedCalls[0].Status != "blocked" {
					t.Fatalf("blocked call executed: guards=%d result=%+v", guards, result)
				}
			} else if guards != 2 || string(tool.lastArgs) != `{"effective":true}` {
				t.Fatalf("normal guards or effective args lost: guards=%d args=%s", guards, tool.lastArgs)
			}
		})
	}
}

func TestNestedToolExecutorNilPreservesGuards(t *testing.T) {
	tool := &recordingTool{}
	ag := NewAgent(nil, "test", "", NewRegistry(orchestrationTool{name: "outer", next: "echo"}, tool))
	ag.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return call.Name != "echo", "denied", nil
	}
	result := ag.CallTool(context.Background(), "root", "outer", json.RawMessage(`{}`), nil)
	if !result.IsError || tool.lastArgs != nil || len(result.NestedCalls) != 1 || result.NestedCalls[0].Executed || result.NestedCalls[0].Status != "blocked" {
		t.Fatalf("nil executor changed nested guards: %+v", result)
	}
}

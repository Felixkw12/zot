package extensions

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
)

func TestDurableCallToolForwardsKeyWithoutWaitDeadline(t *testing.T) {
	m, ext, frames, replies := toolPeer(t)
	m.EnableDurableToolCalls()
	ext.callTool = true
	ext.mu.Lock()
	ext.outboundCalls = map[string]outboundCall{"parent": {ctx: context.Background(), chain: []string{"outer"}}}
	ext.mu.Unlock()
	m.SetToolCaller(func(ctx context.Context, _, _ string, _ json.RawMessage) extproto.ToolResultFromHost {
		if core.NestedToolKey(ctx) != "step-1" {
			t.Errorf("key: %q", core.NestedToolKey(ctx))
		}
		if _, ok := ctx.Deadline(); ok {
			t.Error("durable approval wait has a total deadline")
		}
		if core.NestedToolTimeout(ctx) != 60*time.Second {
			t.Errorf("execution timeout: %v", core.NestedToolTimeout(ctx))
		}
		return extproto.ToolResultFromHost{Content: []extproto.ContentBlock{{Type: "text", Text: "ok"}}}
	})
	sendToolFrame(t, replies, extproto.CallToolFromExt{Type: "call_tool", ID: "one", Name: "read", Args: json.RawMessage(`{}`), ParentID: "parent", Key: "step-1"})
	var result extproto.ToolResultFromHost
	if err := json.Unmarshal(nextLifecycleFrame(t, frames), &result); err != nil || result.IsError {
		t.Fatalf("result: %+v %v", result, err)
	}
	ext.mu.Lock()
	delete(ext.outboundCalls, "parent")
	ext.mu.Unlock()
	sendToolFrame(t, replies, extproto.CallToolFromExt{Type: "call_tool", ID: "two", Name: "read", Args: json.RawMessage(`{}`)})
	if err := json.Unmarshal(nextLifecycleFrame(t, frames), &result); err != nil || !result.IsError {
		t.Fatalf("parentless: %+v %v", result, err)
	}
}

package ext

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

func TestCallToolDurableCapabilityGatesKeys(t *testing.T) {
	for _, durable := range []bool{false, true} {
		name := "legacy"
		if durable {
			name = "durable"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness("caller")
			h.ext.InteractiveTool("ask", "ask", json.RawMessage(`{}`), func(ctx context.Context, _ json.RawMessage) ToolResult {
				result, err := h.ext.CallTool(ctx, "read", map[string]string{"path": "a"}, WithCallKey("step-1"))
				if err != nil {
					return TextErrorResult(err.Error())
				}
				return result
			})
			done := make(chan error, 1)
			go func() { done <- h.ext.Run() }()
			t.Cleanup(func() {
				h.hostW.Close()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(time.Second):
					t.Error("SDK did not exit")
				}
			})
			h.drainUntil(t, "hello")
			caps := []string{"call_tool"}
			if durable {
				caps = append(caps, "call_tool_durable")
			}
			h.sendToExt(t, extproto.HelloAckFromHost{Type: "hello_ack", Capabilities: caps})
			h.drainUntil(t, "ready")
			h.sendToExt(t, extproto.ToolCallFromHost{Type: "tool_call", ID: "parent", Name: "ask", Args: json.RawMessage(`{}`)})
			var call extproto.CallToolFromExt
			if err := json.Unmarshal(h.drainUntil(t, "call_tool").raw, &call); err != nil {
				t.Fatal(err)
			}
			want := ""
			if durable {
				want = "step-1"
			}
			if call.Key != want || call.ParentID != "parent" {
				t.Fatalf("request: %+v", call)
			}
			h.sendToExt(t, extproto.ToolResultFromHost{Type: "tool_result", ID: call.ID, Name: "read", Content: []extproto.ContentBlock{{Type: "text", Text: "loaded"}}})
			h.drainUntil(t, "tool_result")
		})
	}
}

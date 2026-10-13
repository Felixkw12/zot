package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestNestedAuthorizationCallAndRun(t *testing.T) {
	for _, mode := range []string{"call", "run"} {
		for _, policy := range []string{"allowlist", "extension", "hook_error", "rewrite", "missing_parent"} {
			t.Run(mode+"/"+policy, func(t *testing.T) {
				inner := &effectTool{name: "inner"}
				var received core.ToolResult
				outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer"}}
				outer.execute = func(ctx context.Context) (core.ToolResult, error) {
					runtime := core.ToolRuntimeFromContext(ctx)
					if policy == "missing_parent" {
						ctx = context.Background()
					}
					if mode == "call" {
						received = runtime.Call(ctx, "inner", json.RawMessage(`{}`))
					} else {
						received = runtime.Run(ctx, inner, json.RawMessage(`{}`))
					}
					return received, nil
				}
				h := newHarness(t, newMemoryStore(), []scriptStep{{calls: []provider.ToolCallBlock{call("c1", "outer", `{}`)}}, {text: "done"}}, outer, inner)
				defer h.r.Close()
				// Run may execute a host operation not present in the registry.
				if mode == "run" {
					delete(h.tools, "inner")
				}
				cfg := AgentConfig{}
				if policy == "allowlist" {
					cfg.Tools = []string{"outer"}
				}
				hooks, guards := 0, 0
				exts := NewExtensionRegistry()
				if err := exts.Register(Extension{Name: "policy", Hooks: Hooks{BeforeTool: func(_ context.Context, _ Conversation, tc provider.ToolCallBlock) (ToolDecision, error) {
					if tc.Name != "inner" {
						return ToolDecision{}, nil
					}
					hooks++
					switch policy {
					case "extension":
						return ToolDecision{Block: true, Reason: "extension denied"}, nil
					case "hook_error":
						return ToolDecision{}, errors.New("hook failed")
					case "rewrite":
						return ToolDecision{Args: json.RawMessage(`{"extension":true}`)}, nil
					}
					return ToolDecision{}, nil
				}}}); err != nil {
					t.Fatal(err)
				}
				cfg.Extensions = []string{"policy"}
				h.svc.opts.Extensions = exts
				h.guard = func(tc provider.ToolCallBlock) (bool, string, json.RawMessage) {
					if tc.Name == "inner" {
						guards++
						if string(tc.Arguments) != `{"extension":true}` {
							t.Errorf("guard did not see extension rewrite: %s", tc.Arguments)
						}
						return true, "", json.RawMessage(`{"guard":true}`)
					}
					return true, "", nil
				}
				c, err := h.r.OpenRoot(context.Background(), "workspace", cfg)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.r.Submit(context.Background(), c.ID, "actor", "", "hello"); err != nil {
					t.Fatal(err)
				}
				if _, _, err := h.svc.Step(context.Background(), c.ID); err != nil {
					t.Fatal(err)
				}
				if policy == "rewrite" {
					if received.IsError || inner.calls.Load() != 1 || hooks != 1 || guards != 1 || core.ToolResultText(received) != `effect:{"guard":true}` {
						t.Fatalf("authorization rewrite: result=%+v calls=%d hooks=%d guards=%d", received, inner.calls.Load(), hooks, guards)
					}
				} else if !received.IsError || received.Status != "blocked" || inner.calls.Load() != 0 || guards != 0 {
					t.Fatalf("authorization fail-open: result=%+v calls=%d guards=%d", received, inner.calls.Load(), guards)
				}
			})
		}
	}
}

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
)

var extensionCallSeq atomic.Uint64

type extensionOriginKey struct{}

// extensionToolCaller keeps host-initiated calls out of model transcripts,
// while sharing the active agent's guard and confirmation path.
func extensionToolCaller(mgr *extensions.Manager, active func() *core.Agent) func(context.Context, string, string, json.RawMessage) extproto.ToolResultFromHost {
	return func(ctx context.Context, origin, name string, args json.RawMessage) extproto.ToolResultFromHost {
		out := extproto.ToolResultFromHost{Name: name}
		// A nil fallback denotes a continuous host. Its only authority is the
		// runtime carried by a live parent invocation, never a global agent.
		runtime := core.ToolRuntimeFromContext(ctx)
		var ag *core.Agent
		if active == nil {
			if runtime != nil {
				ag = runtime.Agent()
			}
		} else {
			ag = active()
		}
		if ag == nil {
			out.IsError = true
			reason := "no active agent"
			if active == nil {
				reason = "call_tool refused: no resolvable parent tool call in continuous mode"
			}
			out.Content = []extproto.ContentBlock{{Type: "text", Text: reason}}
			return out
		}
		sink := func(ev core.AgentEvent) {
			switch e := ev.(type) {
			case core.EvToolCall:
				mgr.EmitEvent(extproto.EventFromHost{Event: "tool_call", OriginExtension: origin, ToolID: e.ID, ToolName: e.Name, ToolArgs: e.Args})
			case core.EvToolResult:
				mgr.EmitEvent(extproto.EventFromHost{Event: "tool_result", OriginExtension: origin, ToolID: e.ID, ToolName: e.Name, ToolArgs: e.Args, Status: e.Status, Executed: &e.Executed, Result: extensionEventResult(e.Result)})
			}
		}
		ctx = context.WithValue(ctx, extensionOriginKey{}, origin)
		var res core.ToolResult
		if active == nil {
			ctx = core.WithNestedToolOrigin(ctx, origin)
			var untrack []func()
			defer func() {
				for _, done := range untrack {
					done()
				}
			}()
			res = runtime.CallWithEvents(ctx, name, args, func(ev core.AgentEvent) {
				if call, ok := ev.(core.EvToolCall); ok {
					untrack = append(untrack, mgr.TrackToolCall(call.ID, origin))
				}
				sink(ev)
			})
		} else {
			id := fmt.Sprintf("ext-call-%d", extensionCallSeq.Add(1))
			defer mgr.TrackToolCall(id, origin)()
			res = ag.CallTool(ctx, id, name, args, sink)
		}
		visible := extensionEventResult(res)
		out.Content, out.IsError, out.Truncated = visible.Content, visible.IsError, visible.Truncated
		return out
	}
}

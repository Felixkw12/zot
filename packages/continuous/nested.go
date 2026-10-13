package continuous

import (
	"context"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// wireNestedExecution preserves authorization independently of the parent
// execution's disabled guard hooks. The guard view is immutable during execution.
func wireNestedExecution(r *Runtime, agent *core.Agent, conversationID string, exts resolvedExtensions) {
	guard := &core.Agent{
		Tools:                    agent.Tools,
		BeforeToolExecute:        agent.BeforeToolExecute,
		BeforeToolExecuteContext: agent.BeforeToolExecuteContext,
	}
	agent.NestedToolExecutor = func(ctx context.Context, parent, call provider.ToolCallBlock, tool core.Tool, execute func(context.Context, provider.ToolCallBlock) core.ToolResult) core.ToolResult {
		blocked := func(reason string) core.ToolResult {
			return core.ToolResult{IsError: true, Status: "blocked", Content: []provider.Content{provider.TextBlock{Text: reason}}}
		}
		identity, ok := ToolCallFromContext(ctx)
		if !ok || identity.ConversationID != conversationID || identity.RunID == "" || identity.CallID != parent.ID {
			return blocked("nested tool call refused: no resolvable parent call")
		}
		identity.CallID = call.ID
		ctx = context.WithValue(ctx, toolCallKey{}, identity)
		ctx = core.WithToolOperationKey(ctx, identity.OperationKey())
		args, allowed, reason, _ := authorizeCall(ctx, r, guard, conversationID, call, exts, tool)
		if !allowed {
			return blocked(reason)
		}
		call.Arguments = args
		return execute(ctx, call)
	}
}

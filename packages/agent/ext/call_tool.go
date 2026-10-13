package ext

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

type parentCallKey struct{}

// CallToolOption configures one host-mediated call.
type CallToolOption func(*callToolOptions)
type callToolOptions struct{ key string }

// WithCallKey assigns an explicit replay-stable nested key on durable hosts.
// Older hosts do not receive the key and keep their ordinary execution path.
func WithCallKey(key string) CallToolOption {
	return func(options *callToolOptions) { options.key = key }
}

// CallTool asks zot to invoke an active built-in or extension tool. Tool
// failures are returned as ToolResult.IsError, transport failures as err.
// Calls made from tool handlers inherit their parent call for cycle detection
// and cancellation.
func (e *Extension) CallTool(ctx context.Context, name string, args any, options ...CallToolOption) (ToolResult, error) {
	if !e.callToolReady.Load() {
		return ToolResult{}, fmt.Errorf("host does not support call_tool")
	}
	if ctx == nil {
		return ToolResult{}, fmt.Errorf("call_tool requires a context")
	}
	data, err := json.Marshal(args)
	if err != nil {
		return ToolResult{}, fmt.Errorf("encode tool args: %w", err)
	}
	if len(data) == 0 || data[0] != '{' {
		return ToolResult{}, fmt.Errorf("call_tool args must be a JSON object")
	}
	parent, _ := ctx.Value(parentCallKey{}).(string)
	var opts callToolOptions
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}
	if !e.callToolDurable.Load() {
		opts.key = ""
	}
	if !e.callToolDurable.Load() || parent == "" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	id := fmt.Sprintf("sdk-%d", e.callSeq.Add(1))
	ch := make(chan extproto.ToolResultFromHost, 1)
	e.mu.Lock()
	e.pendingCalls[id] = ch
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.pendingCalls, id)
		e.mu.Unlock()
	}()
	if err := e.send(extproto.CallToolFromExt{Type: "call_tool", ID: id, Name: name, Args: data, ParentID: parent, Key: opts.key}); err != nil {
		return ToolResult{}, err
	}
	select {
	case reply := <-ch:
		out := ToolResult{IsError: reply.IsError}
		for _, block := range reply.Content {
			out.Content = append(out.Content, ToolContent{Type: block.Type, Text: block.Text, MimeType: block.MimeType, Data: block.Data})
		}
		if reply.Truncated {
			out.Content = append(out.Content, Text("[tool result truncated by host]"))
		}
		return out, nil
	case <-ctx.Done():
		_ = e.send(extproto.Frame{Type: "call_tool_cancel", ID: id})
		return ToolResult{}, ctx.Err()
	case <-e.done:
		return ToolResult{}, fmt.Errorf("host disconnected")
	}
}

package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"

	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
)

// TrackToolCall records the initiator for confirmation and interception
// events until the call finishes. The returned function removes the entry.
func (m *Manager) TrackToolCall(id, origin string) func() {
	m.mu.Lock()
	if m.callOrigins == nil {
		m.callOrigins = make(map[string]string)
	}
	m.callOrigins[id] = origin
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.callOrigins, id)
		m.mu.Unlock()
	}
}

// ToolCallOrigin returns the initiating extension for an active host call.
func (m *Manager) ToolCallOrigin(id string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.callOrigins[id]
}

// The chain follows a call across extension processes through the host.
// Only host-created parent IDs can carry it into a nested request.
type toolChainKey struct{}

type outboundCall struct {
	ctx   context.Context
	chain []string
}

const maxToolCallDepth = 4

// EnableDurableToolCalls must be set before loading continuous extensions.
// It advertises durable keys and leaves approval waits outside execution timeouts.
func (m *Manager) EnableDurableToolCalls() { m.durableToolCalls.Store(true) }

// SetToolCaller binds host-managed execution to the current agent mode.
// The callback must enforce the same policy as ordinary model tool calls.
func (m *Manager) SetToolCaller(fn func(context.Context, string, string, json.RawMessage) extproto.ToolResultFromHost) {
	m.mu.Lock()
	m.callTool = fn
	m.mu.Unlock()
}

func (m *Manager) handleCallTool(ctx context.Context, ext *Extension, req extproto.CallToolFromExt, cancel context.CancelFunc, parentFound, activeOutbound bool, chain []string) {
	defer cancel()
	defer func() {
		ext.mu.Lock()
		delete(ext.inboundCalls, req.ID)
		ext.mu.Unlock()
	}()
	result := extproto.ToolResultFromHost{Type: "tool_result", ID: req.ID, Name: req.Name}
	fail := func(message string) {
		result.IsError = true
		result.Content = []extproto.ContentBlock{{Type: "text", Text: message}}
	}

	m.mu.RLock()
	caller := m.callTool
	m.mu.RUnlock()
	switch {
	case !ext.callTool:
		fail("extension did not opt in to call_tool")
	case m.durableToolCalls.Load() && (req.ParentID == "" || !parentFound):
		fail("continuous call_tool requires a live parent_id")
	case req.ParentID != "" && !parentFound:
		fail("unknown or expired parent tool call")
	case req.ParentID == "" && activeOutbound:
		fail("parent_id is required while an extension tool is active")
	case slices.Contains(chain, req.Name):
		fail("tool call cycle detected")
	case len(chain) >= maxToolCallDepth:
		fail("tool call depth limit reached")
	case caller == nil:
		fail("host tool calls are unavailable")
	case req.Name == "" || len(req.Args) == 0 || !json.Valid(req.Args) || bytes.TrimSpace(req.Args)[0] != '{':
		fail("call_tool requires a name and JSON object args")
	default:
		ctx = context.WithValue(ctx, toolChainKey{}, append(append([]string(nil), chain...), req.Name))
		if m.durableToolCalls.Load() {
			ctx = core.WithNestedToolKey(ctx, req.Key)
		}
		result = caller(ctx, ext.Manifest.Name, req.Name, req.Args)
		result.Type, result.ID, result.Name = "tool_result", req.ID, req.Name
	}
	frame, err := extproto.Encode(result)
	if err != nil {
		return
	}
	if pipe, ok := ext.stdin.(*orderedPipe); ok {
		_ = pipe.enqueue(frame, nil)
	} else if ext.stdin != nil {
		_, _ = ext.stdin.Write(frame)
	}
}

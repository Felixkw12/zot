package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/patriceckhart/zot/packages/provider"
)

type toolRuntimeKey struct{}

// ToolRuntime lets orchestration tools call other tools through the agent's
// normal execution path without adding intermediate results to the transcript.
// It is scoped to one invocation and must not be retained after Execute returns.
type ToolRuntime struct {
	agent     *Agent
	Tools     []provider.Tool
	State     map[string]json.RawMessage
	call      func(context.Context, string, json.RawMessage, func(AgentEvent)) ToolResult
	run       func(context.Context, Tool, json.RawMessage) ToolResult
	mu        sync.Mutex
	activated []string
	nested    []provider.NestedToolCall
	callIndex map[string]int
}

// ToolRuntimeFromContext returns the capabilities of the current invocation.
// Tools executed outside an agent do not receive a runtime.
func ToolRuntimeFromContext(ctx context.Context) *ToolRuntime {
	runtime, _ := ctx.Value(toolRuntimeKey{}).(*ToolRuntime)
	return runtime
}

// Agent returns the agent that owns this invocation. Hosts can resolve
// callbacks through the parent context rather than a global active agent.
func (r *ToolRuntime) Agent() *Agent { return r.agent }

// Call executes a nested tool with the caller's cancellation context.
func (r *ToolRuntime) Call(ctx context.Context, name string, args json.RawMessage) ToolResult {
	return r.CallWithEvents(ctx, name, args, nil)
}

// CallWithEvents is Call with an additional per-call event observer.
func (r *ToolRuntime) CallWithEvents(ctx context.Context, name string, args json.RawMessage, sink func(AgentEvent)) ToolResult {
	return r.record(r.call(ctx, name, args, sink))
}

func (r *ToolRuntime) record(result ToolResult) ToolResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range result.ActivateTools {
		if !containsString(r.activated, name) {
			r.activated = append(r.activated, name)
		}
	}
	return result
}

// Run executes a host-provided operation without registering or advertising it.
// The operation uses the same guards, nested IDs and events as ordinary calls.
func (r *ToolRuntime) Run(ctx context.Context, tool Tool, args json.RawMessage) ToolResult {
	return r.record(r.run(ctx, tool, args))
}

func (r *ToolRuntime) activatedTools() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.activated...)
}

func (r *ToolRuntime) recordEvent(ev AgentEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch e := ev.(type) {
	case EvToolCall:
		if r.callIndex == nil {
			r.callIndex = map[string]int{}
		}
		if _, exists := r.callIndex[e.ID]; !exists {
			r.callIndex[e.ID] = len(r.nested)
			r.nested = append(r.nested, provider.NestedToolCall{ID: e.ID, Name: e.Name, Args: append(json.RawMessage(nil), e.Args...)})
		}
	case EvToolResult:
		index, exists := r.callIndex[e.ID]
		if !exists {
			return
		}
		call := &r.nested[index]
		call.Args = append(json.RawMessage(nil), e.Args...)
		call.IsError, call.Status, call.Executed = e.Result.IsError, e.Status, e.Executed
		var text []string
		for _, block := range e.Result.Content {
			switch b := block.(type) {
			case provider.TextBlock:
				text = append(text, b.Text)
			case provider.ImageBlock:
				text = append(text, fmt.Sprintf("[image %s, %d bytes]", b.MimeType, len(b.Data)))
			}
		}
		call.Result = strings.Join(text, "\n")
	}
}

func (r *ToolRuntime) nestedCalls() []provider.NestedToolCall {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]provider.NestedToolCall(nil), r.nested...)
}

func (a *Agent) withToolRuntime(ctx context.Context, tc provider.ToolCallBlock, sink func(AgentEvent)) context.Context {
	var chain []string
	if parent := ToolRuntimeFromContext(ctx); parent != nil {
		chain, _ = ctx.Value(toolChainKey{}).([]string)
	}
	chain = append(append([]string(nil), chain...), tc.Name)
	a.mu.Lock()
	specs := a.Tools.AllSpecs()
	state := cloneToolState(a.toolState)
	a.mu.Unlock()
	var seq atomic.Uint64
	var eventMu sync.Mutex
	runtime := &ToolRuntime{agent: a, Tools: specs, State: state}
	execute := func(callCtx context.Context, name string, args json.RawMessage, tool Tool, observer func(AgentEvent)) ToolResult {
		if tool == nil {
			for _, spec := range specs {
				if spec.Name == name && spec.Exposure == "model-only" {
					return ToolResult{IsError: true, Status: "blocked", Content: []provider.Content{provider.TextBlock{Text: "tool is only callable by the model"}}}
				}
			}
		}
		if len(chain) >= 4 || containsString(chain, name) {
			return ToolResult{IsError: true, Status: "blocked", Content: []provider.Content{provider.TextBlock{Text: "nested tool recursion or depth limit exceeded"}}}
		}
		callCtx = context.WithValue(callCtx, toolChainKey{}, chain)
		callCtx = context.WithValue(callCtx, toolRuntimeKey{}, runtime)
		id := fmt.Sprintf("%s/%d", tc.ID, seq.Add(1))
		emit := func(ev AgentEvent) {
			eventMu.Lock()
			defer eventMu.Unlock()
			runtime.recordEvent(ev)
			if sink != nil {
				sink(ev)
			}
			if observer != nil {
				observer(ev)
			}
		}
		call := provider.ToolCallBlock{ID: id, Name: name, Arguments: args}
		emit(EvToolCall{ID: id, Name: name, Args: args})
		if a.NestedToolExecutor == nil {
			return a.runTool(callCtx, call, emit, tool)
		}
		ran := false
		result := a.NestedToolExecutor(callCtx, tc, call, tool, func(ctx context.Context, effective provider.ToolCallBlock) ToolResult {
			ran = true
			return a.runTool(ctx, effective, emit, tool)
		})
		if !ran {
			emit(EvToolResult{ID: id, Name: name, Args: args, Status: result.Status, Result: result})
		}
		return result
	}
	runtime.call = func(ctx context.Context, name string, args json.RawMessage, observer func(AgentEvent)) ToolResult {
		return execute(ctx, name, args, nil, observer)
	}
	runtime.run = func(ctx context.Context, tool Tool, args json.RawMessage) ToolResult {
		if tool == nil {
			return ToolResult{IsError: true, Status: "blocked", Content: []provider.Content{provider.TextBlock{Text: "missing host operation"}}}
		}
		return execute(ctx, tool.Name(), args, tool, nil)
	}
	return context.WithValue(context.WithValue(ctx, toolChainKey{}, chain), toolRuntimeKey{}, runtime)
}

type toolChainKey struct{}

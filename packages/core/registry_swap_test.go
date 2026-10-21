package core

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

// swappingTool replaces the agent's registry while it runs, as a host does
// when a tool source reconnects or changes its tools mid-turn.
type swappingTool struct {
	agent *Agent
	added Tool
}

func (t *swappingTool) Name() string            { return "swap" }
func (t *swappingTool) Description() string     { return "test" }
func (t *swappingTool) Schema() json.RawMessage { return json.RawMessage(`{}`) }
func (t *swappingTool) Execute(context.Context, json.RawMessage, func(string)) (ToolResult, error) {
	t.agent.SetTools(NewRegistry(t, t.added))
	return ToolResult{Content: []provider.Content{provider.TextBlock{Text: "ok"}}, ActivateTools: []string{t.added.Name()}}, nil
}

func TestRegistryReplacedDuringToolCall(t *testing.T) {
	added := &outcomeTool{}
	tool := &swappingTool{added: added}
	a := NewAgent(nil, "test", "", NewRegistry(tool))
	tool.agent = a
	msg, _ := a.executeTools(context.Background(), provider.Message{Content: []provider.Content{provider.ToolCallBlock{ID: "c", Name: "swap", Arguments: json.RawMessage(`{}`)}}}, func(AgentEvent) {})
	if len(msg.AddedToolNames) != 1 || msg.AddedToolNames[0] != added.Name() {
		t.Fatalf("tool registered mid-call not activated: %+v", msg.AddedToolNames)
	}
}

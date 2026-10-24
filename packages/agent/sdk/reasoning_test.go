package sdk

import (
	"context"
	"fmt"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestRuntimeSetReasoningMax(t *testing.T) {
	r := &Runtime{agent: &core.Agent{}}
	if err := r.SetReasoning("max"); err != nil {
		t.Fatal(err)
	}
	if r.agent.Reasoning != "max" {
		t.Fatalf("reasoning = %q, want max", r.agent.Reasoning)
	}
}

func TestRuntimeSetReasoningRejectsUnknownLevel(t *testing.T) {
	r := &Runtime{agent: &core.Agent{}}
	if err := r.SetReasoning("extreme"); err == nil {
		t.Fatal("expected invalid reasoning error")
	}
}

type sdkReasoningClient struct{ wantReasoning bool }

func (sdkReasoningClient) Name() string { return "synthetic" }
func (c sdkReasoningClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	if req.StreamReasoning != c.wantReasoning {
		return nil, fmt.Errorf("reasoning requested=%v, want %v", req.StreamReasoning, c.wantReasoning)
	}
	out := make(chan provider.Event, 3)
	out <- provider.EventReasoningDelta{Delta: "thinking"}
	out <- provider.EventTextDelta{Delta: "answer"}
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "answer"}}}}
	close(out)
	return out, nil
}

func TestSDKReasoningRequiresOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		r := &Runtime{agent: core.NewAgent(sdkReasoningClient{wantReasoning: enabled}, "test", "", nil), showReasoning: enabled}
		events, err := r.Prompt(context.Background(), "hi", nil)
		if err != nil {
			t.Fatal(err)
		}
		var reasoning, text string
		for event := range events {
			switch event.Type {
			case "reasoning_delta":
				reasoning += event.Delta
			case "text_delta":
				text += event.Delta
			}
		}
		if (reasoning != "") != enabled || text != "answer" {
			t.Fatalf("enabled=%v reasoning=%q text=%q", enabled, reasoning, text)
		}
	}
}

func TestReasoningDeltaEvent(t *testing.T) {
	event := toEvent(core.EvReasoningDelta{Delta: "readable"})
	if event.Type != "reasoning_delta" || event.Delta != "readable" {
		t.Fatalf("event = %#v", event)
	}
}

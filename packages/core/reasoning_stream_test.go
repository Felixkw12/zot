package core

import (
	"context"
	"reflect"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

type reasoningStreamClient struct{}

func (reasoningStreamClient) Name() string { return "reasoning-test" }
func (reasoningStreamClient) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	out := make(chan provider.Event, 4)
	out <- provider.EventReasoningDelta{Delta: "first "}
	out <- provider.EventReasoningDelta{Delta: "second"}
	out <- provider.EventTextDelta{Delta: "answer"}
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.ReasoningBlock{Summary: "first second", Encrypted: "opaque"}, provider.TextBlock{Text: "answer"}},
	}}
	close(out)
	return out, nil
}

func TestAgentRequestsReadableReasoningWithoutChangingEffort(t *testing.T) {
	client := &captureClient{}
	a := NewAgent(client, "model", "", Registry{})
	a.Reasoning = "high"
	for _, enabled := range []bool{false, true, false} {
		a.SetStreamReasoning(enabled)
		if err := a.Prompt(context.Background(), "hi", nil, nil); err != nil {
			t.Fatal(err)
		}
		if client.lastReq.StreamReasoning != enabled || client.lastReq.Reasoning != "high" {
			t.Fatalf("enabled=%v request summary=%v effort=%q", enabled, client.lastReq.StreamReasoning, client.lastReq.Reasoning)
		}
	}
}

func TestAgentForwardsReasoningSeparately(t *testing.T) {
	a := NewAgent(reasoningStreamClient{}, "model", "", Registry{})
	var got []string
	err := a.Prompt(context.Background(), "hi", nil, func(ev AgentEvent) {
		switch e := ev.(type) {
		case EvAssistantStart:
			got = append(got, "start")
		case EvReasoningDelta:
			got = append(got, "reasoning:"+e.Delta)
		case EvTextDelta:
			got = append(got, "text:"+e.Delta)
		case EvAssistantMessage:
			got = append(got, "message")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"start", "reasoning:first ", "reasoning:second", "text:answer", "message"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%q, want %q", got, want)
	}
	msgs := a.Messages()
	if len(msgs) != 2 || len(msgs[1].Content) != 2 {
		t.Fatalf("unexpected transcript: %#v", msgs)
	}
	if rb, ok := msgs[1].Content[0].(provider.ReasoningBlock); !ok || rb.Encrypted != "opaque" || rb.Summary != "first second" {
		t.Fatalf("replay metadata changed: %#v", msgs[1].Content[0])
	}
}

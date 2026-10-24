package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/patriceckhart/zot/packages/provider"
)

type readableReasoningClient struct {
	requests []provider.Request
	delta    string
}

func (*readableReasoningClient) Name() string { return "synthetic" }
func (c *readableReasoningClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.requests = append(c.requests, req)
	out := make(chan provider.Event, 2)
	out <- provider.EventReasoningDelta{Delta: c.delta}
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "answer"}},
		Meta:    map[string]string{"existing": "preserved"},
	}}
	close(out)
	return out, nil
}

func TestReadableReasoningSurvivesCompletionAndSessionDecode(t *testing.T) {
	client := &readableReasoningClient{delta: "retained reasoning"}
	a := NewAgent(client, "test", "", nil)
	a.SetStreamReasoning(true)
	var emitted provider.Message
	if err := a.Prompt(context.Background(), "first", nil, func(ev AgentEvent) {
		if event, ok := ev.(EvAssistantMessage); ok {
			emitted = event.Message
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []provider.Message{emitted, a.Messages()[1]} {
		if msg.Meta[provider.ReadableReasoningMetaKey] != client.delta || msg.Meta["existing"] != "preserved" {
			t.Fatalf("missing display metadata: %+v", msg.Meta)
		}
		if len(msg.Content) != 1 {
			t.Fatal("reasoning was added to model content")
		}
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := DecodeMessage(raw)
		if err != nil || restored.ReadableReasoning() != client.delta {
			t.Fatalf("session lost reasoning: %+v, err=%v", restored, err)
		}
	}
	if err := a.Prompt(context.Background(), "second", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, msg := range client.requests[1].Messages {
		if _, exists := msg.Meta[provider.ReadableReasoningMetaKey]; exists {
			t.Fatal("display-only reasoning leaked into model input")
		}
	}
	if a.Messages()[1].ReadableReasoning() != client.delta {
		t.Fatal("request preparation changed the saved transcript")
	}
}

func TestReadableReasoningPersistenceIsBoundedAndOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		client := &readableReasoningClient{delta: strings.Repeat("古", 20000) + "latest"}
		a := NewAgent(client, "test", "", nil)
		a.SetStreamReasoning(enabled)
		if err := a.Prompt(context.Background(), "first", nil, nil); err != nil {
			t.Fatal(err)
		}
		text := a.Messages()[1].Meta[provider.ReadableReasoningMetaKey]
		if !enabled {
			if text != "" {
				t.Fatal("default stream persisted display-only reasoning")
			}
			continue
		}
		if !utf8.ValidString(text) || len([]rune(text)) != 8192 || !strings.HasSuffix(text, "latest") {
			t.Fatalf("invalid persisted suffix: runes=%d", len([]rune(text)))
		}
	}
}

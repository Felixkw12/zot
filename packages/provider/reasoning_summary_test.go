package provider

import (
	"encoding/json"
	"testing"
)

func TestAnthropicRequestsReadableThinking(t *testing.T) {
	for _, model := range []string{"claude-fable-5-1", "claude-opus-4-7", "claude-sonnet-4-5"} {
		for _, effort := range []string{"", "high"} {
			for _, enabled := range []bool{false, true} {
				c := NewAnthropic("test", "").(*anthropicClient)
				wire, err := c.buildRequest(Request{Model: model, Reasoning: effort, StreamReasoning: enabled})
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(wire)
				if err != nil {
					t.Fatal(err)
				}
				var body struct {
					Thinking *struct {
						Type    string `json:"type"`
						Display string `json:"display"`
					} `json:"thinking"`
				}
				if err := json.Unmarshal(data, &body); err != nil {
					t.Fatal(err)
				}
				if effort == "" {
					if body.Thinking != nil {
						t.Fatalf("model=%s: display opt-in enabled thinking without effort", model)
					}
					continue
				}
				want := ""
				if enabled {
					want = "summarized"
				}
				if body.Thinking == nil || body.Thinking.Display != want {
					t.Errorf("model=%s enabled=%v: thinking=%+v, want display=%q", model, enabled, body.Thinking, want)
				}
			}
		}
	}
}

func TestThinkingDisplayNotSentToCompatibleProviders(t *testing.T) {
	c := NewAnthropic("test", "").(*anthropicClient)
	c.name = "compatible-proxy"
	wire, err := c.buildRequest(Request{Model: "claude-fable-5-1", Reasoning: "high", StreamReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Thinking == nil {
		t.Fatal("expected configured thinking")
	}
	if wire.Thinking.Display != "" {
		t.Fatal("Anthropic display option leaked to compatible provider")
	}
}

func TestResponsesRequestsReadableReasoningSummaries(t *testing.T) {
	for _, providerName := range []string{"openai-codex", "openai-responses"} {
		for _, effort := range []string{"", "high"} {
			for _, enabled := range []bool{false, true} {
				c := &codexClient{providerName: providerName}
				wire, err := c.buildRequest(Request{Model: "gpt-6.1-sol", Reasoning: effort, StreamReasoning: enabled})
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(wire)
				if err != nil {
					t.Fatal(err)
				}
				var body struct {
					Reasoning struct {
						Effort  string `json:"effort"`
						Summary string `json:"summary"`
					} `json:"reasoning"`
				}
				if err := json.Unmarshal(data, &body); err != nil {
					t.Fatal(err)
				}
				wantSummary := ""
				if enabled {
					wantSummary = "auto"
				}
				if body.Reasoning.Summary != wantSummary || body.Reasoning.Effort != effort {
					t.Errorf("provider=%s enabled=%v effort=%q: reasoning=%+v", providerName, enabled, effort, body.Reasoning)
				}
			}
		}
	}
}

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadableReasoningStreamsBeforeCompletion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client func(string) Client
		model  string
		prefix string
		suffix string
	}{
		{
			name:   "openai compatible",
			client: func(url string) Client { return NewOpenAI("test", url) },
			model:  "local-model",
			prefix: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"first \"}}]}\n\n",
			suffix: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"second\",\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		},
		{
			name:   "anthropic",
			client: func(url string) Client { return NewAnthropic("test", url) },
			model:  "claude-fable-5-1",
			prefix: "event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"first \"}}\n\n",
			suffix: "event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"second\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"private-signature\"}}\n\nevent: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\nevent: message_stop\ndata: {}\n\n",
		},
		{
			name:   "responses",
			client: func(url string) Client { return NewOpenAIResponses("test", url) },
			model:  "gpt-5.4",
			prefix: `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"r1","encrypted_content":"private-encrypted"}}

data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"first "}

`,
			suffix: `data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"second"}

data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message"}}

data: {"type":"response.output_text.delta","output_index":1,"delta":"answer"}

`,
		},
		{
			name:   "gemini",
			client: func(url string) Client { return NewGemini("test", url) },
			model:  "gemini-2.5-pro",
			prefix: "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"first \",\"thought\":true,\"thoughtSignature\":\"private-signature\"}]}}]}\n\n",
			suffix: "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"second\",\"thought\":true},{\"text\":\"answer\"}]},\"finishReason\":\"STOP\"}]}\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			received := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.name == "anthropic" {
					var body struct {
						Thinking struct {
							Type    string `json:"type"`
							Display string `json:"display"`
						} `json:"thinking"`
						OutputConfig struct {
							Effort string `json:"effort"`
						} `json:"output_config"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Thinking.Type != "adaptive" || body.Thinking.Display != "summarized" || body.OutputConfig.Effort != "high" {
						t.Errorf("readable high-effort thinking was not requested: %+v, err=%v", body, err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}
				if tc.name == "responses" {
					var body struct {
						Reasoning struct {
							Summary string `json:"summary"`
						} `json:"reasoning"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Reasoning.Summary != "auto" {
						t.Error("readable reasoning summary was not requested")
						return
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.prefix)
				w.(http.Flusher).Flush()
				select {
				case <-received:
				case <-ctx.Done():
					return
				}
				fmt.Fprint(w, tc.suffix)
			}))
			defer server.Close()
			events, err := tc.client(server.URL).Stream(ctx, Request{Model: tc.model, Reasoning: "high", StreamReasoning: true, Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}}})
			if err != nil {
				t.Fatal(err)
			}
			var reasoning, text strings.Builder
			var done bool
			for event := range events {
				switch e := event.(type) {
				case EventReasoningDelta:
					if reasoning.Len() == 0 {
						close(received)
					}
					reasoning.WriteString(e.Delta)
				case EventTextDelta:
					text.WriteString(e.Delta)
				case EventDone:
					done = true
					if e.Err != nil {
						t.Error(e.Err)
					}
				}
			}
			if reasoning.String() != "first second" || text.String() != "answer" || !done {
				t.Fatalf("reasoning=%q text=%q done=%v", reasoning.String(), text.String(), done)
			}
		})
	}
}

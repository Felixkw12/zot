package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiniMaxAnthropicThinkingReplayHTTP(t *testing.T) {
	for _, id := range []string{"MiniMax-M3.1-Flash", "MiniMax-M3"} {
		t.Run(id, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if id == "MiniMax-M3" && body["output_config"] != nil {
					t.Error("M3 effort leaked")
				}
				thinking, _ := body["thinking"].(map[string]any)
				if thinking["type"] != "adaptive" || thinking["budget_tokens"] != nil {
					t.Errorf("thinking %v", thinking)
				}
				if requests == 2 {
					messages := body["messages"].([]any)
					assistant := messages[1].(map[string]any)["content"].([]any)
					block := assistant[0].(map[string]any)
					if block["type"] != "thinking" || block["thinking"] != "first second" || block["signature"] != "sig" {
						t.Errorf("replay %v", block)
					}
					if assistant[1].(map[string]any)["type"] != "tool_use" {
						t.Error("tool order lost")
					}
					fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
					return
				}
				fmt.Fprint(w, `event: content_block_start
data: {"index":0,"content_block":{"type":"thinking","thinking":"first "}}

event: content_block_delta
data: {"index":0,"delta":{"type":"thinking_delta","thinking":"second"}}

event: content_block_delta
data: {"index":0,"delta":{"type":"signature_delta","signature":"sig"}}

event: content_block_start
data: {"index":1,"content_block":{"type":"tool_use","id":"call-1","name":"test"}}

event: content_block_delta
data: {"index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}

event: message_stop
data: {}

`)
			}))
			defer server.Close()
			c := NewAnthropicCompat("minimax", "test-key", server.URL)
			req := Request{Model: id, Reasoning: "high", Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hello"}}}}}
			events, err := c.Stream(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			var answer Message
			for event := range events {
				if done, ok := event.(EventDone); ok {
					if done.Err != nil {
						t.Fatal(done.Err)
					}
					answer = done.Message
				}
			}
			if len(answer.Content) != 2 {
				t.Fatalf("lost thinking or tool: %+v", answer)
			}
			req.Messages = append(req.Messages, answer, Message{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "call-1", Content: []Content{TextBlock{Text: "done"}}}}})
			assertMiniMaxThinkingNotReplayed(t, req.Messages)
			events, err = c.Stream(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			for event := range events {
				if done, ok := event.(EventDone); ok && done.Err != nil {
					t.Fatal(done.Err)
				}
			}
			if requests != 2 {
				t.Fatalf("requests %d", requests)
			}
		})
	}
}

func assertMiniMaxThinkingNotReplayed(t *testing.T, messages []Message) {
	t.Helper()
	for _, route := range []struct {
		name, model string
		client      func(string) Client
	}{
		{"codex", "gpt-5.5", func(url string) Client { return NewOpenAICodex("test-key", "test-account", url) }},
		{"responses", "gpt-5.5", func(url string) Client { return NewOpenAIResponses("test-key", url) }},
		{"gemini", "gemini-2.5-pro", func(url string) Client { return NewGemini("test-key", url) }},
	} {
		t.Run("switch/"+route.name, func(t *testing.T) {
			bodies := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				bodies <- string(body)
				w.Header().Set("Content-Type", "text/event-stream")
			}))
			defer server.Close()
			events, err := route.client(server.URL).Stream(context.Background(), Request{Model: route.model, Messages: messages})
			if err != nil {
				t.Fatal(err)
			}
			for event := range events {
				if done, ok := event.(EventDone); ok && done.Err != nil {
					t.Fatal(done.Err)
				}
			}
			body := <-bodies
			for _, foreign := range []string{"minimax-thinking", "first second", `"sig"`} {
				if strings.Contains(body, foreign) {
					t.Errorf("foreign thinking %q leaked into %s request", foreign, route.name)
				}
			}
			for _, retained := range []string{`"hello"`, `"test"`, `"done"`} {
				if !strings.Contains(body, retained) {
					t.Errorf("conversation content %s lost on provider switch", retained)
				}
			}
		})
	}
}

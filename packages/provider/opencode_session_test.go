package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenCodeGoSessionHeader(t *testing.T) {
	for _, tc := range []struct {
		model string
		path  string
	}{
		{"deepseek-v4.1-flash", "/v1/chat/completions"},
		{"gpt-5.6-luna", "/v1/responses"},
		{"gpt-6-luna", "/v1/responses"},
		{"grok-4.7", "/v1/responses"},
		{"muse-spark-1.2-contributor", "/v1/responses"},
		{"muse-spark-1.3-contributor", "/v1/responses"},
		{"claude-haiku-5-5", "/anthropic/v1/messages"},
		{"minimax-m3", "/anthropic/v1/messages"},
		{"qwen3.7-plus", "/anthropic/v1/messages"},
		{"qwen3.8-flash", "/anthropic/v1/messages"},
		{"qwen3.8-max", "/anthropic/v1/messages"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			type request struct{ path, session string }
			requests := make(chan request, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- request{r.URL.Path, r.Header.Get("x-opencode-session")}
				w.Header().Set("Content-Type", "text/event-stream")
			}))
			defer server.Close()
			client := NewOpenCodeGo("key", server.URL)
			for _, session := range []string{"conversation-one", "conversation-two"} {
				events, err := client.Stream(context.Background(), Request{
					Model: tc.model, SessionID: session,
					Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
				})
				if err != nil {
					t.Fatal(err)
				}
				for range events {
				}
				select {
				case got := <-requests:
					if got.path != tc.path || got.session != session {
						t.Fatalf("request = %+v, want path %q and session %q", got, tc.path, session)
					}
				default:
					t.Fatal("request was not sent")
				}
			}
		})
	}
}

func TestOpenCodeSessionHeaderOtherProviders(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		newClient   func(string, string) Client
	}{
		{"openai", "gpt-5", NewOpenAI},
		{"opencode", "kimi-k3", NewOpenCode},
		{"anthropic", "claude-sonnet-4-5", NewAnthropic},
		{"openai-responses", "gpt-5.6-luna", NewOpenAIResponses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headers <- r.Header.Get("x-opencode-session")
				w.Header().Set("Content-Type", "text/event-stream")
			}))
			defer server.Close()
			client := tc.newClient("key", server.URL)
			events, err := client.Stream(context.Background(), Request{
				Model: tc.model, SessionID: "conversation",
				Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			for range events {
			}
			select {
			case got := <-headers:
				if got != "" {
					t.Fatalf("x-opencode-session = %q, want empty", got)
				}
			default:
				t.Fatal("request was not sent")
			}
		})
	}
}

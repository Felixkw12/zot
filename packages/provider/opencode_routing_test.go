package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenCodeRoutesResponsesModels(t *testing.T) {
	for _, tc := range []struct{ model, path string }{
		{"gpt-5.6-luna", "/v1/responses"},
		{"gpt-6-luna", "/v1/responses"},
		{"kimi-k3", "/v1/chat/completions"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			paths := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths <- r.URL.Path
				w.Header().Set("Content-Type", "text/event-stream")
			}))
			defer server.Close()
			client := NewOpenCode("key", server.URL+"/v1/")
			events, err := client.Stream(context.Background(), Request{
				Model:    tc.model,
				Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			for range events {
			}
			if got := <-paths; got != tc.path {
				t.Fatalf("request path = %q, want %q", got, tc.path)
			}
		})
	}
}

func TestOpenCodeGoAnthropicBaseURL(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"", "https://opencode.ai/inference/go/anthropic"},
		{"https://opencode.ai/zen/go/v1/", "https://opencode.ai/inference/go/anthropic"},
		{"https://opencode.ai/inference/go/openai/v1", "https://opencode.ai/inference/go/anthropic"},
		{"https://proxy.example/go/v1", "https://proxy.example/go/anthropic"},
		{"https://proxy.example/go/v1/", "https://proxy.example/go/anthropic"},
		{"https://proxy.example/go", "https://proxy.example/go/anthropic"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			router := NewOpenCodeGo("key", tc.base).(*modelRouter)
			client := router.byAPI[APIAnthropicMessages].(*anthropicClient)
			if client.baseURL != tc.want {
				t.Fatalf("Anthropic base URL = %q, want %q", client.baseURL, tc.want)
			}
		})
	}
}

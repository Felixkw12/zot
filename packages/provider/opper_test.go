package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpperChatCompletionsURL(t *testing.T) {
	client := NewOpper("test-key", "").(*openaiClient)
	const want = "https://api.opper.ai/v3/compat/chat/completions"
	if got := client.chatCompletionsURL(); got != want {
		t.Fatalf("chat completions URL = %q, want %q", got, want)
	}
}

func TestOpperStreamEndpoint(t *testing.T) {
	for _, suffix := range []string{"/v3/compat", "/v3/compat/", "/openai/v1"} {
		t.Run(suffix, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantPath := strings.TrimRight(suffix, "/") + "/chat/completions"
				if r.Method != http.MethodPost || r.URL.Path != wantPath {
					t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.Path, wantPath)
					http.NotFound(w, r)
					return
				}
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("incorrect authorization")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			client := NewOpper("test-key", server.URL+suffix)
			events, err := client.Stream(context.Background(), Request{Model: "claude-sonnet-4-6"})
			if err != nil {
				t.Fatal(err)
			}
			for range events {
			}
		})
	}
}

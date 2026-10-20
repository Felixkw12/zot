package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestMiniMaxDefaultsHTTP(t *testing.T) {
	for _, p := range []string{"minimax", "minimax-cn"} {
		for _, saved := range []string{"", "MiniMax-M3", "MiniMax-M2.7"} {
			t.Run(p+"/"+saved, func(t *testing.T) {
				t.Setenv("ZOT_HOME", t.TempDir())
				t.Setenv("MINIMAX_API_KEY", "test-key")
				t.Setenv("MINIMAX_CN_API_KEY", "test-key")
				if err := SaveConfig(Config{Provider: p, Model: saved}); err != nil {
					t.Fatal(err)
				}
				r, err := Resolve(Args{NoTools: true, NoContextFiles: true}, false)
				if err != nil {
					t.Fatal(err)
				}
				expected := saved
				if expected == "" {
					expected = "MiniMax-M3.1-Flash"
				}
				if r.Model != expected {
					t.Fatalf("resolved %s want %s", r.Model, expected)
				}
				called := false
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					called = true
					var body map[string]any
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					if body["model"] != expected {
						t.Errorf("wire model %v", body["model"])
					}
					if saved == "" {
						config, _ := body["output_config"].(map[string]any)
						if config["effort"] != "max" {
							t.Errorf("default effort %v", config)
						}
					}
					fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
				}))
				defer server.Close()
				r.BaseURL = server.URL
				events, err := r.NewClient().Stream(context.Background(), provider.Request{Model: r.Model, Reasoning: r.Reasoning, MaxTokens: r.MaxOutput, Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hello"}}}}})
				if err != nil {
					t.Fatal(err)
				}
				for range events {
				}
				if !called {
					t.Fatal("request did not reach HTTP boundary")
				}
			})
		}
	}
	if got := defaultModelForProvider("openrouter"); got != "anthropic/claude-sonnet-4.5" {
		t.Errorf("unrelated default changed: %s", got)
	}
}

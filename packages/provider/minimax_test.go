package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestMiniMaxOrdinaryCatalog(t *testing.T) {
	SetLiveModels(nil)
	t.Cleanup(func() { SetLiveModels(nil); SetUserModels(nil) })
	for _, p := range []string{"minimax", "minimax-cn"} {
		models := ModelsForProvider(p)
		var ids []string
		for _, m := range models {
			ids = append(ids, m.ID)
		}
		if !reflect.DeepEqual(ids, []string{"MiniMax-M3.1-Flash", "MiniMax-M3"}) {
			t.Fatalf("%s: %v", p, ids)
		}
		m, ok := FindModel(p, "MiniMax-M2.7")
		if ok != nil || m.ID != "MiniMax-M2.7" {
			t.Fatal("saved legacy model no longer resolves")
		}
	}
	for _, m := range Active() {
		if id := miniMaxTextID(m); id != "" && id != "minimax-m3" && !miniMaxFlash(m) {
			t.Errorf("legacy choice visible: %s/%s", m.Provider, m.ID)
		}
	}
	custom := Model{Provider: "minimax", ID: "MiniMax-M2.7", Source: "user"}
	media := Model{Provider: "minimax", ID: "music-2.6"}
	unrelated := Model{Provider: "openai", ID: "gpt-5"}
	customProvider := Model{Provider: "my-gateway", ID: "MiniMax-M2.7"}
	if got := filterAvailableModels([]Model{custom, media, unrelated, customProvider}); !reflect.DeepEqual(got, []Model{custom, media, unrelated, customProvider}) {
		t.Fatalf("custom/media/unrelated entries changed: %+v", got)
	}
}

func TestMiniMaxFlashHTTP(t *testing.T) {
	for _, route := range []struct {
		provider, id string
		anthropic    bool
	}{
		{"minimax", "MiniMax-M3.1-Flash", true},
		{"minimax-cn", "MiniMax-M3.1-Flash", true},
		{"minimax", "MiniMax-M3.1-Flash", false},
		{"openrouter", "minimax/minimax-m3.1-flash:free", false},
		{"opencode", "minimax-m3.1-flash-free", false},
	} {
		for _, effort := range []string{"", "off", "low", "medium", "high", "xhigh", "max"} {
			t.Run(route.provider+fmt.Sprint(route.anthropic)+"/"+effort, func(t *testing.T) {
				m, ok := FindModel(route.provider, route.id)
				if ok != nil {
					t.Fatal("missing model")
				}
				expected := effort
				if expected == "" || expected == "off" {
					expected = "max"
				}
				if got := AvailableReasoningLevels(m); !reflect.DeepEqual(got, []string{"low", "medium", "high", "xhigh", "max"}) {
					t.Fatalf("levels %v", got)
				}
				called := false
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					if body["model"] != route.id {
						t.Errorf("model %v", body["model"])
					}
					if route.anthropic {
						if r.URL.Path != "/v1/messages" {
							t.Errorf("path %s", r.URL.Path)
						}
						thinking, _ := body["thinking"].(map[string]any)
						if thinking["type"] != "adaptive" || thinking["budget_tokens"] != nil {
							t.Errorf("thinking %v", thinking)
						}
						config, _ := body["output_config"].(map[string]any)
						if config["effort"] != expected {
							t.Errorf("effort %v want %s", config, expected)
						}
						if body["max_tokens"] != float64(m.MaxOutput) {
							t.Errorf("max_tokens %v", body["max_tokens"])
						}
						fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					} else {
						if r.URL.Path != "/v1/chat/completions" {
							t.Errorf("path %s", r.URL.Path)
						}
						if body["reasoning_effort"] != expected {
							t.Errorf("effort %v want %s", body["reasoning_effort"], expected)
						}
						if body["max_completion_tokens"] != float64(m.MaxOutput) {
							t.Errorf("max_completion_tokens %v", body["max_completion_tokens"])
						}
						if body["reasoning_split"] == false {
							t.Error("disabled reasoning split")
						}
						fmt.Fprint(w, "data: [DONE]\n\n")
					}
				}))
				defer server.Close()
				var c Client
				if route.anthropic {
					c = NewAnthropicCompat(route.provider, "test-key", server.URL)
				} else {
					c = NewOpenAICompat(route.provider, "test-key", server.URL, "")
				}
				events, err := c.Stream(context.Background(), Request{Model: route.id, Reasoning: effort, Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hello"}}}}})
				if err != nil {
					t.Fatal(err)
				}
				for event := range events {
					if done, ok := event.(EventDone); ok && done.Err != nil {
						t.Fatal(done.Err)
					}
				}
				if !called {
					t.Fatal("HTTP client did not send request")
				}
			})
		}
	}
}

func TestMiniMaxM3AndLegacyThinking(t *testing.T) {
	c := NewAnthropicCompat("minimax", "test-key", "http://localhost").(*anthropicClient)
	for _, effort := range []string{"", "high"} {
		req, err := c.buildRequest(Request{Model: "MiniMax-M3", Reasoning: effort})
		if err != nil {
			t.Fatal(err)
		}
		if req.OutputConfig != nil {
			t.Fatal("M3 must omit output_config.effort")
		}
		if effort == "" && req.Thinking != nil {
			t.Fatal("M3 off must omit Anthropic thinking")
		}
		if effort != "" && (req.Thinking == nil || req.Thinking.Type != "adaptive" || req.Thinking.BudgetTokens != 0) {
			t.Fatal("M3 must use adaptive thinking without budget")
		}
	}
	oai := NewOpenAICompat("minimax", "test-key", "http://localhost", "").(*openaiClient)
	for _, effort := range []string{"", "high"} {
		wire, err := oai.buildRequest(Request{Model: "MiniMax-M3", Reasoning: effort})
		if err != nil {
			t.Fatal(err)
		}
		expected := "adaptive"
		if effort == "" {
			expected = "disabled"
		}
		if wire.Thinking == nil || wire.Thinking.Type != expected || wire.ReasoningEffort != "" {
			t.Fatalf("M3 OpenAI thinking: %+v", wire)
		}
	}
	req, err := c.buildRequest(Request{Model: "MiniMax-M2.7", Reasoning: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if req.Thinking == nil || req.Thinking.Type != "enabled" || req.Thinking.BudgetTokens == 0 {
		t.Fatal("legacy saved request changed")
	}
}

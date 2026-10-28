package provider

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestDiscoverOpper(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/compat/models" || r.URL.Query().Get("view") != "simplified" {
			t.Errorf("unexpected endpoint: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("incorrect authorization")
		}
		fmt.Fprint(w, `{"object":"list","data":[
			{"id":"claude-sonnet-4-6","object":"model","created":0,"owned_by":"opper","context_length":1000000,"pricing":{"prompt":"0.0000033","completion":"0.0000165","input_cache_read":"0.00000033","input_cache_write":"0.000004125"},"opper":{"kind":"pool","type":"llm","display_name":"Claude Sonnet 4.6","capabilities":["text","tools","vision","reasoning"],"max_output_tokens":32000,"reasoning":{"supported":[]}}},
			{"id":"gpt-5.4-mini","object":"model","created":0,"owned_by":"opper","context_length":400000,"pricing":{"prompt":"0.00000075","completion":"0.0000045"},"opper":{"kind":"pool","type":"llm","capabilities":["text","tools","reasoning"],"max_output_tokens":128000,"reasoning":{"supported":["low","medium","high"]}}},
			{"id":"new-chat","object":"model","created":0,"owned_by":"opper","opper":{"kind":"model","type":"llm","capabilities":["text","tools"]}},
			{"id":"claude-sonnet-4-6","object":"model","created":0,"owned_by":"opper","opper":{"kind":"model","type":"llm","capabilities":["text","tools"]}},
			{"id":"","object":"model","created":0,"owned_by":"opper","opper":{"kind":"model","type":"llm","capabilities":["text","tools"]}},
			{"id":"text-embedding-3-small","object":"model","created":0,"owned_by":"opper","opper":{"kind":"pool","type":"embedding","capabilities":["text"]}},
			{"id":"no-tools","object":"model","created":0,"owned_by":"opper","opper":{"kind":"model","type":"llm","capabilities":["text"]}}
		]}`)
	}))
	defer server.Close()
	models, err := DiscoverOpper(context.Background(), "test-key", server.URL+"/v3/compat/")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
	m := models[0]
	if m.Provider != "opper" || m.ID != "claude-sonnet-4-6" || m.DisplayName != "Claude Sonnet 4.6" || m.Source != "live" || m.BaseURL != server.URL+"/v3/compat" || !slices.Equal(m.Input, []string{"text", "image"}) {
		t.Errorf("model identity = %+v", m)
	}
	if m.ContextWindow != 1000000 || m.MaxOutput != 32000 || m.Reasoning || !near(m.PriceInput, 3.3) || !near(m.PriceOutput, 16.5) || !near(m.PriceCacheRead, 0.33) || !near(m.PriceCacheWrite, 4.125) {
		t.Errorf("model metadata = %+v", m)
	}
	if models[1].ID != "gpt-5.4-mini" || models[1].DisplayName != "gpt-5.4-mini" || !models[1].Reasoning || !slices.Equal(models[1].Input, []string{"text"}) {
		t.Errorf("reasoning model = %+v", models[1])
	}
	if models[2].ContextWindow != customDiscoveryContextWindow || models[2].MaxOutput != customDiscoveryMaxOutput || models[2].Reasoning {
		t.Errorf("fallback metadata = %+v", models[2])
	}
	SetLiveModels(models)
	t.Cleanup(func() { SetLiveModels(nil) })
	seed, err := FindModel("opper", "claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	if seed.MaxOutput != 32000 || !near(seed.PriceInput, 3.3) {
		t.Errorf("seed metadata was not replaced: %+v", seed)
	}
	client := NewOpper("test-key", server.URL+"/v3/compat").(*openaiClient)
	wire, err := client.buildRequest(Request{Model: models[1].ID, Reasoning: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Model != models[1].ID || wire.MaxCompletionTok == nil || *wire.MaxCompletionTok != 128000 || wire.ReasoningEffort != "high" {
		t.Errorf("wire request = %+v", wire)
	}
}

func TestDiscoverOpperErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, "private-response"},
		{"invalid-json", http.StatusOK, "private-response"},
		{"missing-data", http.StatusOK, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			_, err := DiscoverOpper(context.Background(), "test-key", server.URL)
			if err == nil || strings.Contains(err.Error(), "private-response") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverOpper(ctx, "test-key", "http://127.0.0.1:1"); err == nil {
		t.Fatal("canceled discovery succeeded")
	}
}

func TestOpperPrice(t *testing.T) {
	for _, raw := range []string{"", "invalid", "-0.000001", "NaN", "+Inf"} {
		if got := opperPrice(raw); got != 0 {
			t.Errorf("price(%q) = %v", raw, got)
		}
	}
	if got := opperPrice("0.000003"); !near(got, 3) {
		t.Errorf("price(0.000003) = %v, want 3", got)
	}
}

func near(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

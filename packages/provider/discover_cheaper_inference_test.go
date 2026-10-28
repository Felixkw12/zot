package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestDiscoverCheaperInference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.URL.Query().Get("type") != "text" || r.URL.Query().Get("streaming") != "true" {
			t.Errorf("unexpected endpoint: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("incorrect authorization")
		}
		fmt.Fprint(w, `{"data":[
			{"id":"claude-fable-5","type":"text","endpoint":"/v1/messages","supported_endpoints":["/v1/messages","/v1/chat/completions"],"context_length":200000,"max_output_tokens":64000,"capabilities":{"vision":true,"reasoning":true,"streaming":true},"pricing":{"input_per_million":"6.5","output_per_million":"32.5","cache_read_input_per_million":"0.65","cache_write_input_per_million":"8.125"}},
			{"id":"new-chat","type":"text","endpoint":"/v1/chat/completions","context_length":null,"max_output_tokens":null,"capabilities":{"streaming":true}},
			{"id":"gpt-5.4-mini","type":"text","endpoint":"/v1/chat/completions","context_length":100000,"max_output_tokens":16000,"capabilities":{"streaming":true},"pricing":{"input_per_million":"0.4"}},
			{"id":"claude-fable-5","type":"text","endpoint":"/v1/chat/completions","capabilities":{"streaming":true}},
			{"id":"","type":"text","endpoint":"/v1/chat/completions","capabilities":{"streaming":true}},
			{"id":"image-model","type":"image","endpoint":"/v1/images/generations","capabilities":{"streaming":true}},
			{"id":"embedding-model","type":"embedding","endpoint":"/v1/embeddings"},
			{"id":"responses-only","type":"text","endpoint":"/v1/chat/completions","supported_endpoints":["/v1/responses"],"capabilities":{"streaming":true}},
			{"id":"no-streaming","type":"text","endpoint":"/v1/chat/completions","capabilities":{"streaming":false}}
		]}`)
	}))
	defer server.Close()
	models, err := DiscoverCheaperInference(context.Background(), "test-key", server.URL+"/v1/")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
	m := models[0]
	if m.Provider != "cheaperinference" || m.ID != "claude-fable-5" || m.Source != "live" || m.BaseURL != server.URL+"/v1" || !slices.Equal(m.Input, []string{"text", "image"}) {
		t.Errorf("model identity = %+v", m)
	}
	if m.ContextWindow != 200000 || m.MaxOutput != 64000 || !m.Reasoning || m.PriceInput != 6.5 || m.PriceOutput != 32.5 || m.PriceCacheRead != 0.65 || m.PriceCacheWrite != 8.125 {
		t.Errorf("model metadata = %+v", m)
	}
	if models[1].ContextWindow != 32768 || models[1].MaxOutput != 4096 || models[1].Reasoning {
		t.Errorf("fallback metadata = %+v", models[1])
	}
	SetLiveModels(models)
	t.Cleanup(func() { SetLiveModels(nil) })
	seed, err := FindModel("cheaperinference", "gpt-5.4-mini")
	if err != nil {
		t.Fatal(err)
	}
	if seed.ContextWindow != 100000 || seed.MaxOutput != 16000 || seed.Reasoning || seed.PriceInput != 0.4 {
		t.Errorf("seed metadata was not replaced: %+v", seed)
	}
	client := NewCheaperInference("test-key", server.URL+"/v1").(*openaiClient)
	wire, err := client.buildRequest(Request{Model: m.ID, Reasoning: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Model != m.ID || wire.MaxCompletionTok == nil || *wire.MaxCompletionTok != 64000 || wire.ReasoningEffort != "high" {
		t.Errorf("wire request = %+v", wire)
	}
}

func TestDiscoverCheaperInferenceErrors(t *testing.T) {
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
			_, err := DiscoverCheaperInference(context.Background(), "test-key", server.URL)
			if err == nil || strings.Contains(err.Error(), "private-response") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverCheaperInference(ctx, "test-key", "http://127.0.0.1:1"); err == nil {
		t.Fatal("canceled discovery succeeded")
	}
}

func TestCheaperInferencePrice(t *testing.T) {
	for _, raw := range []string{"", "invalid", "-1", "NaN", "+Inf"} {
		if got := cheaperInferencePrice(raw); got != 0 {
			t.Errorf("price(%q) = %v", raw, got)
		}
	}
}

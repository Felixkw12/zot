package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCustomModelsURL(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:48000":          "http://127.0.0.1:48000/v1/models",
		"http://127.0.0.1:48000/":         "http://127.0.0.1:48000/v1/models",
		"http://127.0.0.1:48000/v1":       "http://127.0.0.1:48000/v1/models",
		"http://127.0.0.1:48000/v1/":      "http://127.0.0.1:48000/v1/models",
		"https://llm.example.com/paas/v4": "https://llm.example.com/paas/v4/models",
	}
	for in, want := range cases {
		got, err := customModelsURL(in)
		if err != nil || got != want {
			t.Errorf("customModelsURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "localhost:1234", "ftp://x", "/v1"} {
		if _, err := customModelsURL(bad); err == nil {
			t.Errorf("customModelsURL(%q) accepted", bad)
		}
	}
}

func TestDiscoverCustomProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer dummy" {
			t.Errorf("missing bearer header: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"qwen3-coder"},{"id":"nomic-embed-text"},{"id":"qwen3-coder"},{"id":""},{"id":"gpt-oss-20b"}]}`)
	}))
	defer server.Close()

	models, err := DiscoverCustomProvider(context.Background(), "m4", server.URL+"/v1", "dummy")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	first := models[0]
	if first.Provider != "m4" || first.ID != "qwen3-coder" || first.Source != "live" || first.BaseURL != server.URL+"/v1" {
		t.Fatalf("model = %+v", first)
	}
	if first.ContextWindow != customDiscoveryContextWindow || first.MaxOutput != customDiscoveryMaxOutput {
		t.Fatalf("defaults = %d/%d", first.ContextWindow, first.MaxOutput)
	}
	if models[1].ID != "gpt-oss-20b" {
		t.Fatalf("second model = %+v", models[1])
	}
}

func TestDiscoverCustomProviderReadsReportedContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"omlx","owned_by":"omlx","max_model_len":196600},
			{"id":"router","context_length":131072},
			{"id":"gateway","context_window":65536},
			{"id":"tiny","max_model_len":2048},
			{"id":"bad-string","max_model_len":"196600"},
			{"id":"bad-zero","max_model_len":0,"context_length":8192},
			{"id":"bad-fraction","max_model_len":1.5},
			{"id":"plain"}
		]}`)
	}))
	defer server.Close()
	models, err := DiscoverCustomProvider(context.Background(), "m5", server.URL+"/v1", "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int{
		"omlx":         {196600, customDiscoveryMaxOutput},
		"router":       {131072, customDiscoveryMaxOutput},
		"gateway":      {65536, customDiscoveryMaxOutput},
		"tiny":         {2048, 2048},
		"bad-string":   {customDiscoveryContextWindow, customDiscoveryMaxOutput},
		"bad-zero":     {8192, customDiscoveryMaxOutput},
		"bad-fraction": {customDiscoveryContextWindow, customDiscoveryMaxOutput},
		"plain":        {customDiscoveryContextWindow, customDiscoveryMaxOutput},
	}
	if len(models) != len(want) {
		t.Fatalf("models = %+v", models)
	}
	for _, m := range models {
		w := want[m.ID]
		if m.ContextWindow != w[0] || m.MaxOutput != w[1] {
			t.Errorf("%s limits = %d/%d, want %d/%d", m.ID, m.ContextWindow, m.MaxOutput, w[0], w[1])
		}
	}
}

func TestDiscoverCustomProviderOmitsHeaderWithoutKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Error("unexpected Authorization header")
		}
		fmt.Fprint(w, `{"data":[{"id":"local"}]}`)
	}))
	defer server.Close()
	models, err := DiscoverCustomProvider(context.Background(), "m5", server.URL, "")
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%+v err=%v", models, err)
	}
}

func TestDiscoverCustomProviderErrors(t *testing.T) {
	status := http.StatusUnauthorized
	body := `{}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer server.Close()

	if _, err := DiscoverCustomProvider(context.Background(), "m4", server.URL, "k"); err == nil || err.Error() != "m4 discovery HTTP 401" {
		t.Fatalf("status error = %v", err)
	}
	status = http.StatusOK
	body = `{"object":"list"}`
	if _, err := DiscoverCustomProvider(context.Background(), "m4", server.URL, "k"); err == nil {
		t.Fatal("missing data accepted")
	}
	body = `not json`
	if _, err := DiscoverCustomProvider(context.Background(), "m4", server.URL, "k"); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if _, err := DiscoverCustomProvider(context.Background(), "", server.URL, "k"); err == nil {
		t.Fatal("empty provider id accepted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverCustomProvider(ctx, "m4", server.URL, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

package agent

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestOpperPreservesUncachedSelection(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPPER_API_KEY", "test-key")
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })
	for _, id := range []string{"kimi-k3", "anthropic/claude-sonnet-4-6"} {
		if err := SaveConfig(Config{Provider: "opper", Model: id}); err != nil {
			t.Fatal(err)
		}
		ValidateAndRepairConfig()
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Model != id {
			t.Errorf("saved model = %q, want %q", cfg.Model, id)
		}
		for _, explicit := range []string{"", id} {
			r, err := Resolve(Args{Provider: "opper", Model: explicit, CWD: t.TempDir(), NoTools: true, NoContextFiles: true}, true)
			if err != nil {
				t.Fatal(err)
			}
			if r.Model != id || r.Provider != "opper" {
				t.Errorf("resolved %s/%s, want opper/%s", r.Provider, r.Model, id)
			}
			if r.MaxOutput != 4096 {
				t.Errorf("fallback output limit = %d, want 4096", r.MaxOutput)
			}
			if r.Credential != "test-key" || r.NewClient().Name() != "opper" {
				t.Error("provider credential or client wiring is incorrect")
			}
		}
	}
}

func TestOpperRefreshWithFreshCache(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPPER_API_KEY", "test-key")
	t.Setenv("YOLO_AUTO_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("CHEAPER_INFERENCE_API_KEY", "")
	cachedAt := time.Now().UTC()
	if err := provider.SaveCache(ModelCachePath(), provider.ModelCache{FetchedAt: cachedAt, Models: []provider.Model{
		{Provider: "openai", ID: "unrelated-cached-model"},
		{Provider: "opper", ID: "previous-account-model"},
	}}); err != nil {
		t.Fatal(err)
	}
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	fail := false
	http.DefaultTransport = yoloAutoTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.opper.ai" || r.URL.Path != "/v3/compat/models" {
			return nil, fmt.Errorf("unexpected discovery request: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("incorrect authorization")
		}
		calls++
		if fail {
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("unauthorized")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"gpt-5.5","object":"model","created":0,"owned_by":"opper","context_length":400000,"pricing":{"prompt":"0.000005","completion":"0.00003"},"opper":{"kind":"pool","type":"llm","display_name":"GPT-5.5","capabilities":["text","tools","vision","reasoning"],"max_output_tokens":128000,"reasoning":{"supported":["low","medium","high"]}}}]}`))}, nil
	})
	refreshModels()
	if calls != 1 {
		t.Fatalf("discovery calls = %d, want 1", calls)
	}
	m, err := provider.FindModel("opper", "gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	if m.ContextWindow != 400000 || m.MaxOutput != 128000 || !m.Reasoning || math.Abs(m.PriceInput-5) > 1e-9 {
		t.Errorf("discovered metadata = %+v", m)
	}
	if _, err := provider.FindModel("opper", "previous-account-model"); err == nil {
		t.Error("previous account model survived refresh")
	}
	if _, err := provider.FindModel("openai", "unrelated-cached-model"); err != nil {
		t.Error(err)
	}
	cached, err := provider.LoadCache(ModelCachePath())
	if err != nil {
		t.Fatal(err)
	}
	if !cached.FetchedAt.Equal(cachedAt) {
		t.Error("refresh extended unrelated cache lifetime")
	}
	fail = true
	refreshModels()
	if _, err := provider.FindModel("opper", "gpt-5.5"); err == nil {
		t.Error("account model survived failed discovery")
	}
	fail = false
	refreshModels()
	t.Setenv("OPPER_API_KEY", "")
	refreshModels()
	if _, err := provider.FindModel("opper", "gpt-5.5"); err == nil {
		t.Error("account model survived logout")
	}
	if _, err := provider.FindModel("opper", defaultModelForProvider("opper")); err != nil {
		t.Error("offline default missing:", err)
	}
}

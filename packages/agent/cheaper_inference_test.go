package agent

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestCheaperInferencePreservesUncachedSelection(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("CHEAPER_INFERENCE_API_KEY", "test-key")
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })
	for _, id := range []string{"claude-fable-5", "vendor/new-model"} {
		if err := SaveConfig(Config{Provider: "cheaperinference", Model: id}); err != nil {
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
			r, err := Resolve(Args{Provider: "cheaperinference", Model: explicit, CWD: t.TempDir(), NoTools: true, NoContextFiles: true}, true)
			if err != nil {
				t.Fatal(err)
			}
			if r.Model != id || r.Provider != "cheaperinference" {
				t.Errorf("resolved %s/%s, want cheaperinference/%s", r.Provider, r.Model, id)
			}
			if r.MaxOutput != 4096 {
				t.Errorf("fallback output limit = %d, want 4096", r.MaxOutput)
			}
			if r.Credential != "test-key" || r.NewClient().Name() != "cheaperinference" {
				t.Error("provider credential or client wiring is incorrect")
			}
		}
	}
}

func TestCheaperInferenceRefreshWithFreshCache(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("CHEAPER_INFERENCE_API_KEY", "test-key")
	t.Setenv("YOLO_AUTO_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("OPPER_API_KEY", "")
	cachedAt := time.Now().UTC()
	if err := provider.SaveCache(ModelCachePath(), provider.ModelCache{FetchedAt: cachedAt, Models: []provider.Model{
		{Provider: "openai", ID: "unrelated-cached-model"},
		{Provider: "cheaperinference", ID: "previous-account-model"},
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
		if r.URL.Host != "api.cheaperinference.com" || r.URL.Path != "/v1/models" {
			return nil, fmt.Errorf("unexpected discovery request: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("incorrect authorization")
		}
		calls++
		if fail {
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("unauthorized")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"claude-fable-5","type":"text","endpoint":"/v1/chat/completions","supported_endpoints":["/v1/chat/completions"],"context_length":200000,"max_output_tokens":64000,"capabilities":{"streaming":true,"reasoning":true},"pricing":{"currency":"USD","input_per_million":"6.5","output_per_million":"32.5"}}]}`))}, nil
	})
	refreshModels()
	if calls != 1 {
		t.Fatalf("discovery calls = %d, want 1", calls)
	}
	m, err := provider.FindModel("cheaperinference", "claude-fable-5")
	if err != nil {
		t.Fatal(err)
	}
	if m.ContextWindow != 200000 || m.MaxOutput != 64000 || !m.Reasoning || m.PriceInput != 6.5 {
		t.Errorf("discovered metadata = %+v", m)
	}
	if _, err := provider.FindModel("cheaperinference", "previous-account-model"); err == nil {
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
	if _, err := provider.FindModel("cheaperinference", "claude-fable-5"); err == nil {
		t.Error("account model survived failed discovery")
	}
	fail = false
	refreshModels()
	t.Setenv("CHEAPER_INFERENCE_API_KEY", "")
	refreshModels()
	if _, err := provider.FindModel("cheaperinference", "claude-fable-5"); err == nil {
		t.Error("account model survived logout")
	}
	if _, err := provider.FindModel("cheaperinference", defaultModelForProvider("cheaperinference")); err != nil {
		t.Error("offline default missing:", err)
	}
}

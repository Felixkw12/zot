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

type yoloAutoTestTransport func(*http.Request) (*http.Response, error)

func (f yoloAutoTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestYoloAutoDefaultAvailableOffline(t *testing.T) {
	id := defaultModelForProvider("yolo-auto")
	if id != "qwen3.8-flash" {
		t.Fatalf("default = %q, want free-plan model", id)
	}
	if _, err := provider.FindModel("yolo-auto", id); err != nil {
		t.Fatal(err)
	}
}

func TestYoloAutoRefreshIgnoresSharedCache(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("CHEAPER_INFERENCE_API_KEY", "")
	t.Setenv("OPPER_API_KEY", "")
	t.Setenv("YOLO_AUTO_API_KEY", "test-pro")
	cachedAt := time.Now().UTC()
	if err := provider.SaveCache(ModelCachePath(), provider.ModelCache{
		FetchedAt: cachedAt,
		Models: []provider.Model{
			{Provider: "openai", ID: "cached-public-model"},
			{Provider: "yolo-auto", ID: "old-account-model", ContextWindow: 262144},
		},
	}); err != nil {
		t.Fatal(err)
	}
	provider.SetLiveModels(nil)
	t.Cleanup(func() { provider.SetLiveModels(nil) })
	LoadCachedModels()

	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	calls := 0
	http.DefaultTransport = yoloAutoTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://yolo-auto.com/v1/models" {
			return nil, fmt.Errorf("unexpected discovery request: %s", r.URL)
		}
		calls++
		body := `{"data":[{"id":"qwen3.8-flash","context_length":262144,"thinking":["high"]},{"id":"pro-only"}]}`
		switch r.Header.Get("Authorization") {
		case "Bearer test-pro":
		case "Bearer test-free":
			body = `{"data":[{"id":"qwen3.8-flash","context_length":131072,"thinking":["high"]}]}`
		default:
			return nil, fmt.Errorf("unexpected credential")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})

	for _, tc := range []struct {
		key    string
		window int
	}{
		{"test-pro", 262144},
		{"test-free", 131072},
	} {
		t.Setenv("YOLO_AUTO_API_KEY", tc.key)
		refreshModels()
		model, err := provider.FindModel("yolo-auto", "qwen3.8-flash")
		if err != nil {
			t.Fatal(err)
		}
		if model.ContextWindow != tc.window {
			t.Errorf("context = %d, want %d", model.ContextWindow, tc.window)
		}
	}
	if calls != 2 {
		t.Errorf("discovery calls = %d, want 2 despite fresh cache", calls)
	}
	if _, err := provider.FindModel("yolo-auto", "pro-only"); err == nil {
		t.Error("previous account model survived refresh")
	}
	cached, err := provider.LoadCache(ModelCachePath())
	if err != nil {
		t.Fatal(err)
	}
	if !cached.FetchedAt.Equal(cachedAt) {
		t.Error("account refresh extended public cache lifetime")
	}
	for _, model := range cached.Models {
		if model.Provider == "yolo-auto" && (model.ID != "qwen3.8-flash" || model.ContextWindow != 131072) {
			t.Errorf("previous account metadata survived in cache: %+v", model)
		}
	}
	if _, err := provider.FindModel("yolo-auto", "old-account-model"); err == nil {
		t.Error("previous cached account model survived refresh")
	}
	if _, err := provider.FindModel("openai", "cached-public-model"); err != nil {
		t.Error("unrelated cached model lost")
	}

	// Failed discovery and logout must drop the previous account's metadata.
	for _, key := range []string{"test-invalid", ""} {
		t.Setenv("YOLO_AUTO_API_KEY", "test-pro")
		refreshModels()
		t.Setenv("YOLO_AUTO_API_KEY", key)
		refreshModels()
		model, err := provider.FindModel("yolo-auto", "qwen3.8-flash")
		if err != nil {
			t.Fatal(err)
		}
		if model.Source != "catalog" || len(model.ReasoningLevelMap) != 0 {
			t.Errorf("account metadata survived failed discovery or logout: %+v", model)
		}
		if _, err := provider.FindModel("yolo-auto", "pro-only"); err == nil {
			t.Error("previous account model survived failed discovery or logout")
		}
	}
}

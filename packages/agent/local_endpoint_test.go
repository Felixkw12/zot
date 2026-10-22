package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestResolveLocalProviderAfterLogout(t *testing.T) {
	for _, name := range []string{provider.LlamaCPPProviderID, provider.LMStudioProviderID} {
		for _, apiKey := range []string{"", "explicit-test-key"} {
			t.Run(name+"/key="+apiKey, func(t *testing.T) {
				t.Setenv("ZOT_HOME", t.TempDir())
				t.Setenv("LLAMA_BASE_URL", "")
				t.Setenv("LLAMA_API_KEY", "")
				t.Setenv("LMSTUDIO_API_KEY", "")
				const model = "issue-246-local-model"
				if err := SaveConfig(Config{Provider: name, Model: model}); err != nil {
					t.Fatal(err)
				}
				store := AuthStoreFor()
				if err := store.SetEndpointCredential(name, "http://127.0.0.1:8080", ""); err != nil {
					t.Fatal(err)
				}
				if err := store.Clear(name); err != nil {
					t.Fatal(err)
				}
				args := Args{APIKey: apiKey, NoSkill: true, CWD: t.TempDir()}
				r, err := Resolve(args, false)
				if err != nil {
					t.Fatalf("interactive resolution after logout: %v", err)
				}
				if r.Provider != name || r.Model != model {
					t.Fatalf("selection changed to %s/%s", r.Provider, r.Model)
				}
				if r.BaseURL != "" || r.HasCredential() {
					t.Fatalf("missing endpoint must not enable a client: URL=%q, has credential=%v", r.BaseURL, r.HasCredential())
				}
				for _, inferenceOnly := range []bool{false, true} {
					_, err := resolve(context.Background(), args, true, inferenceOnly)
					if err == nil || !strings.Contains(err.Error(), "server URL") || !strings.Contains(err.Error(), "/login") {
						t.Fatalf("inferenceOnly=%v: expected actionable missing endpoint error, got %v", inferenceOnly, err)
					}
				}

				args.BaseURL = "http://127.0.0.1:8080/v1"
				r, err = Resolve(args, true)
				if err != nil {
					t.Fatalf("explicit endpoint: %v", err)
				}
				if r.BaseURL != args.BaseURL || !r.HasCredential() {
					t.Fatal("explicit endpoint did not enable local inference")
				}
			})
		}
	}
}

func TestResolveLocalProviderRejectsMalformedStoredURL(t *testing.T) {
	for _, name := range []string{provider.LlamaCPPProviderID, provider.LMStudioProviderID} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ZOT_HOME", t.TempDir())
			t.Setenv("LLAMA_BASE_URL", "")
			t.Setenv("LLAMA_API_KEY", "")
			if err := AuthStoreFor().SetEndpointCredential(name, "file:///invalid", ""); err != nil {
				t.Fatal(err)
			}
			_, err := Resolve(Args{Provider: name, Model: "local-model", NoSkill: true, CWD: t.TempDir()}, false)
			if err == nil || !strings.Contains(err.Error(), "http or https") {
				t.Fatalf("expected URL validation error, got %v", err)
			}
		})
	}
}

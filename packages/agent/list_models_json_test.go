package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestParseArgsListModelsJSON(t *testing.T) {
	args, err := ParseArgs([]string{"--list-models-json"})
	if err != nil {
		t.Fatal(err)
	}
	if !args.ListModelsJSON || args.ListModels {
		t.Fatal("JSON listing must be separate from text listing")
	}
}

func TestModelPickerCatalogFiltersByProvider(t *testing.T) {
	models := []provider.Model{
		{Provider: "openai", ID: "gpt-a"},
		{Provider: "anthropic", ID: "claude-a"},
		{Provider: "kimi", ID: "kimi-a"},
	}
	got := modelPickerCatalog(models, []string{"anthropic", "openai"})
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Provider == "kimi" {
			t.Fatalf("kimi is not a picker provider, but was listed: %+v", e)
		}
	}
}

func TestModelPickerCatalogSortsByProviderThenID(t *testing.T) {
	models := []provider.Model{
		{Provider: "openai", ID: "zeta"},
		{Provider: "anthropic", ID: "beta"},
		{Provider: "openai", ID: "alpha"},
		{Provider: "anthropic", ID: "alpha"},
	}
	got := modelPickerCatalog(models, []string{"anthropic", "openai"})
	want := []string{"anthropic/alpha", "anthropic/beta", "openai/alpha", "openai/zeta"}
	if len(got) != len(want) {
		t.Fatalf("want %d entries, got %d", len(want), len(got))
	}
	for i, e := range got {
		if e.Provider+"/"+e.ID != want[i] {
			t.Fatalf("index %d: want %s, got %s/%s", i, want[i], e.Provider, e.ID)
		}
	}
}

func TestModelPickerCatalogSourceDefaults(t *testing.T) {
	models := []provider.Model{
		{Provider: "openai", ID: "plain"},
		{Provider: "openai", ID: "user", Source: "user"},
		{Provider: "openai", ID: "guess", Speculative: true},
	}
	got := modelPickerCatalog(models, []string{"openai"})
	byID := map[string]string{}
	for _, e := range got {
		byID[e.ID] = e.Source
	}
	for id, want := range map[string]string{"plain": "catalog", "user": "user", "guess": "speculative"} {
		if byID[id] != want {
			t.Errorf("source for %s = %q, want %q", id, byID[id], want)
		}
	}
}

func TestModelPickerCatalogEmptyMarshalsAsArray(t *testing.T) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(modelPickerCatalog(nil, nil)); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "[]" {
		t.Fatalf("empty catalog: want [], got %q", buf.String())
	}
}

func TestPrintModelCatalogJSONUsesSnakeCaseKeys(t *testing.T) {
	entries := modelPickerCatalog([]provider.Model{{
		Provider: "openai", ID: "gpt-x", DisplayName: "GPT X",
		Reasoning: true, ContextWindow: 100, MaxOutput: 10,
	}}, []string{"openai"})
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"provider"`, `"id"`, `"name"`, `"reasoning"`, `"context_window"`, `"max_output"`, `"source"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("missing key %s in %s", key, raw)
		}
	}
}

func TestPrintModelCatalogJSONWaitsForDiscovery(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("LLAMA_BASE_URL", "")
	t.Setenv("LLAMA_API_KEY", "")
	t.Setenv("JSON_CUSTOM_API_KEY", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			fmt.Fprint(w, `{"models":[{"type":"llm","key":"json-lmstudio"}]}`)
		case "/models":
			fmt.Fprint(w, `{"data":[{"id":"json-llama","status":{"value":"loaded"}}]}`)
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"json-custom"}]}`)
		default:
			t.Errorf("unexpected discovery path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers":{"json-custom":{"baseUrl":%q,"discover":true}}}`, server.URL+"/v1"))
	for _, p := range []string{provider.LMStudioProviderID, provider.LlamaCPPProviderID} {
		if err := AuthStoreFor().SetEndpointCredential(p, server.URL, ""); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := printModelCatalogJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var entries []catalogEntry
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"lmstudio/json-lmstudio", "llama.cpp/json-llama", "json-custom/json-custom"} {
		found := false
		for _, e := range entries {
			if e.Provider+"/"+e.ID == want {
				found = true
			}
		}
		if !found {
			t.Errorf("discovered model %s missing from JSON", want)
		}
	}
}

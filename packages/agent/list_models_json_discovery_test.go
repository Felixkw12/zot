package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider/auth"
)

func TestListModelsJSONCLIIncludesDiscovery(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("LLAMA_BASE_URL", "")
	t.Setenv("CLI_CUSTOM_API_KEY", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"id":"cli-discovered"}]}`)
	}))
	defer server.Close()
	body := fmt.Sprintf(`{"providers":{"cli-custom":{"baseUrl":%q,"discover":true}}}`, server.URL+"/v1")
	if err := os.WriteFile(UserModelsPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunHelpHelperProcess$")
	cmd.Env = append(os.Environ(), "ZOT_HELP_HELPER=--list-models-json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("CLI failed: %v, stderr: %s", err, &stderr)
	}
	var entries []catalogEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("stdout is not a JSON array: %v", err)
	}
	for _, e := range entries {
		if e.Provider == "cli-custom" && e.ID == "cli-discovered" {
			return
		}
	}
	t.Fatalf("CLI omitted discovered model, stderr: %s", &stderr)
}

func TestPrintModelCatalogJSONDiscoveryFailureKeepsJSON(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("LLAMA_BASE_URL", "")
	t.Setenv("JSON_CUSTOM_API_KEY", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers":{"json-custom":{"baseUrl":%q,"discover":true,"models":[{"id":"pinned"}]}}}`, server.URL+"/v1"))
	var stdout, stderr bytes.Buffer
	if err := printModelCatalogJSONContext(context.Background(), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), `"pinned"`) {
		t.Fatal("discovery failure corrupted JSON or dropped configured model")
	}
	if !strings.Contains(stderr.String(), "custom providers model discovery:") {
		t.Fatalf("missing discovery diagnostic: %s", &stderr)
	}
}

func TestPrintModelCatalogJSONDiscoveryHonorsCancellation(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("LLAMA_BASE_URL", "")
	t.Setenv("JSON_CUSTOM_API_KEY", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers":{"json-custom":{"baseUrl":%q,"discover":true}}}`, server.URL+"/v1"))
	var stdout, stderr bytes.Buffer
	if err := printModelCatalogJSONContext(ctx, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stdout.Bytes()) || !strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("expected JSON and cancellation diagnostic, stderr: %s", &stderr)
	}
}

func TestPrintModelCatalogJSONSkipsCredentialCommands(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("LLAMA_BASE_URL", "")
	t.Setenv("LLAMA_API_KEY", "")
	t.Setenv("JSON_CUSTOM_API_KEY", "")
	t.Setenv("ZOT_AGENT_API_KEY_COMMAND_HELPER", "1")
	marker := filepath.Join(t.TempDir(), "command-ran")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("command-backed discovery must not contact the server")
	}))
	defer server.Close()
	loadTestModelsJSON(t, fmt.Sprintf(`{"providers":{"json-custom":{"baseUrl":%q,"discover":true,"models":[{"id":"pinned"}]}}}`, server.URL+"/v1"))
	creds := auth.Credentials{AdditionalAPIKeyCreds: map[string]auth.ProviderCreds{}}
	for _, p := range []string{"llama.cpp", "lmstudio", "json-custom"} {
		creds.AdditionalAPIKeyCreds[p] = auth.ProviderCreds{
			BaseURL: server.URL,
			APIKeyCommand: &auth.APIKeyCommand{
				Program: os.Args[0],
				Args:    []string{"-test.run=^TestAgentAPIKeyCommandHelperProcess$", "--", marker},
			},
		}
	}
	data, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := printModelCatalogJSONContext(context.Background(), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("credential command ran: %v", err)
	}
	if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), `"pinned"`) || stderr.Len() != 0 {
		t.Fatalf("expected configured model without errors, stderr: %s", &stderr)
	}
}

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Conservative metadata for models that a list endpoint describes by ID
// only. A context length reported by the endpoint replaces the context
// default, and explicit models.json entries override both.
const (
	customDiscoveryContextWindow = 32768
	customDiscoveryMaxOutput     = 4096
)

// customModelsURL derives the list endpoint from a custom provider base URL.
// A base that already ends in an API version segment such as "/v1" gets
// "/models" appended; a bare host gets "/v1/models".
func customModelsURL(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("custom provider base URL must be an http or https URL")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	base := u.String()
	if versionSegmentSuffix.MatchString(base) {
		return base + "/models", nil
	}
	return base + "/v1/models", nil
}

// DiscoverCustomProvider lists models from a user-defined provider's
// OpenAI-style /models endpoint. Besides the ID, only a context length
// reported as max_model_len (vLLM, oMLX), context_length or
// context_window is read; otherwise context and output limits use
// conservative defaults because self-hosted gateways rarely report them
// consistently. Entries whose ID
// mentions embeddings are skipped. Discovered models carry the provider's
// base URL so they route like models.json entries.
func DiscoverCustomProvider(ctx context.Context, providerID, baseURL, apiKey string) ([]Model, error) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return nil, fmt.Errorf("custom provider discovery requires a provider id")
	}
	endpoint, err := customModelsURL(baseURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s discovery request failed", providerID)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s discovery HTTP %d", providerID, resp.StatusCode)
	}
	var page struct {
		Data []struct {
			ID            string          `json:"id"`
			MaxModelLen   json.RawMessage `json:"max_model_len"`
			ContextLength json.RawMessage `json:"context_length"`
			ContextWindow json.RawMessage `json:"context_window"`
		} `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<20)).Decode(&page); err != nil {
		return nil, fmt.Errorf("%s discovery returned invalid model data", providerID)
	}
	if page.Data == nil {
		return nil, fmt.Errorf("%s discovery response is missing data", providerID)
	}
	var models []Model
	seen := map[string]bool{}
	for _, d := range page.Data {
		id := strings.TrimSpace(d.ID)
		if id == "" || seen[id] || strings.Contains(strings.ToLower(id), "embed") {
			continue
		}
		seen[id] = true
		contextWindow := customDiscoveryContextWindow
		for _, raw := range []json.RawMessage{d.MaxModelLen, d.ContextLength, d.ContextWindow} {
			if n := discoveredTokenLimit(raw); n > 0 {
				contextWindow = n
				break
			}
		}
		models = append(models, Model{
			Provider:      providerID,
			ID:            id,
			DisplayName:   id,
			ContextWindow: contextWindow,
			MaxOutput:     min(customDiscoveryMaxOutput, contextWindow),
			BaseURL:       strings.TrimRight(strings.TrimSpace(baseURL), "/"),
			Source:        "live",
		})
	}
	return models, nil
}

// discoveredTokenLimit decodes a token count reported by a custom /models
// endpoint. Missing, non-numeric, fractional, or non-positive values yield
// 0 so one malformed field never fails discovery.
func discoveredTokenLimit(raw json.RawMessage) int {
	var n float64
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil || n < 1 || n > math.MaxInt32 || n != math.Trunc(n) {
		return 0
	}
	return int(n)
}

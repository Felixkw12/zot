package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
)

// DiscoverOpper lists the tool-capable chat models an Opper key can use, from
// the OpenAI-compatible /models list in its simplified view: each model once,
// under its pool's bare name when several providers serve it. A pool reports
// the smallest limits of its members and the most expensive member's per-token
// prices. Embedding models and models without tool calling are excluded.
func DiscoverOpper(ctx context.Context, apiKey, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = opperDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models?view=simplified", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opper discover: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("opper discover HTTP %d", resp.StatusCode)
	}
	var page struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt          string `json:"prompt"`
				Completion      string `json:"completion"`
				InputCacheRead  string `json:"input_cache_read"`
				InputCacheWrite string `json:"input_cache_write"`
			} `json:"pricing"`
			Opper struct {
				Type            string   `json:"type"`
				DisplayName     string   `json:"display_name"`
				Capabilities    []string `json:"capabilities"`
				MaxOutputTokens int      `json:"max_output_tokens"`
				Reasoning       *struct {
					Supported []string `json:"supported"`
				} `json:"reasoning"`
			} `json:"opper"`
		} `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 16<<20)).Decode(&page); err != nil {
		return nil, fmt.Errorf("opper discover returned invalid model data")
	}
	if page.Data == nil {
		return nil, fmt.Errorf("opper discover response is missing data")
	}
	models := make([]Model, 0, len(page.Data))
	seen := make(map[string]bool)
	for _, d := range page.Data {
		id := strings.TrimSpace(d.ID)
		if id == "" || seen[id] || d.Opper.Type != "llm" || !slices.Contains(d.Opper.Capabilities, "tools") {
			continue
		}
		seen[id] = true
		window, output := d.ContextLength, d.Opper.MaxOutputTokens
		if window <= 0 {
			window = customDiscoveryContextWindow
		}
		if output <= 0 {
			output = customDiscoveryMaxOutput
		}
		input := []string{"text"}
		if slices.Contains(d.Opper.Capabilities, "vision") {
			input = append(input, "image")
		}
		display := strings.TrimSpace(d.Opper.DisplayName)
		if display == "" {
			display = id
		}
		models = append(models, Model{
			Provider:         "opper",
			ID:               id,
			DisplayName:      display,
			Input:            input,
			ContextWindow:    window,
			MaxOutput:        output,
			Reasoning:        d.Opper.Reasoning != nil && len(d.Opper.Reasoning.Supported) > 0,
			AdaptiveThinking: usesAdaptiveThinking(Model{ID: id}),
			PriceInput:       opperPrice(d.Pricing.Prompt),
			PriceOutput:      opperPrice(d.Pricing.Completion),
			PriceCacheRead:   opperPrice(d.Pricing.InputCacheRead),
			PriceCacheWrite:  opperPrice(d.Pricing.InputCacheWrite),
			BaseURL:          baseURL,
			Source:           "live",
		})
	}
	return models, nil
}

// opperPrice converts Opper's per-token USD price strings to USD per 1M
// tokens. Missing, negative and non-finite values become 0.
func opperPrice(raw string) float64 {
	price := perMillionTokens(raw)
	if price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0
	}
	return price
}

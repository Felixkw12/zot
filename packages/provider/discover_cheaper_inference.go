package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DiscoverCheaperInference lists streaming Chat Completions models visible to
// the key. Media models and models restricted to other wire APIs are excluded.
// The gateway reports token prices directly in USD per million tokens.
func DiscoverCheaperInference(ctx context.Context, apiKey, baseURL string) ([]Model, error) {
	if baseURL == "" {
		baseURL = cheaperInferenceDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models?type=text&streaming=true", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cheaperinference discover: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cheaperinference discover HTTP %d", resp.StatusCode)
	}
	var page struct {
		Data []struct {
			ID                 string   `json:"id"`
			Type               string   `json:"type"`
			Endpoint           string   `json:"endpoint"`
			SupportedEndpoints []string `json:"supported_endpoints"`
			ContextLength      int      `json:"context_length"`
			MaxOutputTokens    int      `json:"max_output_tokens"`
			Capabilities       struct {
				Vision    bool `json:"vision"`
				Reasoning bool `json:"reasoning"`
				Streaming bool `json:"streaming"`
			} `json:"capabilities"`
			Pricing struct {
				Input      string `json:"input_per_million"`
				Output     string `json:"output_per_million"`
				CacheRead  string `json:"cache_read_input_per_million"`
				CacheWrite string `json:"cache_write_input_per_million"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<20)).Decode(&page); err != nil {
		return nil, fmt.Errorf("cheaperinference discover returned invalid model data")
	}
	if page.Data == nil {
		return nil, fmt.Errorf("cheaperinference discover response is missing data")
	}
	models := make([]Model, 0, len(page.Data))
	seen := make(map[string]bool)
	for _, d := range page.Data {
		id := strings.TrimSpace(d.ID)
		chat := slices.Contains(d.SupportedEndpoints, "/v1/chat/completions")
		if d.SupportedEndpoints == nil {
			chat = d.Endpoint == "/v1/chat/completions"
		}
		if id == "" || seen[id] || d.Type != "text" || !d.Capabilities.Streaming || !chat {
			continue
		}
		seen[id] = true
		window, output := d.ContextLength, d.MaxOutputTokens
		if window <= 0 {
			window = customDiscoveryContextWindow
		}
		if output <= 0 {
			output = customDiscoveryMaxOutput
		}
		input := []string{"text"}
		if d.Capabilities.Vision {
			input = append(input, "image")
		}
		models = append(models, Model{
			Provider:         "cheaperinference",
			ID:               id,
			DisplayName:      id,
			Input:            input,
			ContextWindow:    window,
			MaxOutput:        output,
			Reasoning:        d.Capabilities.Reasoning,
			AdaptiveThinking: usesAdaptiveThinking(Model{ID: id}),
			PriceInput:       cheaperInferencePrice(d.Pricing.Input),
			PriceOutput:      cheaperInferencePrice(d.Pricing.Output),
			PriceCacheRead:   cheaperInferencePrice(d.Pricing.CacheRead),
			PriceCacheWrite:  cheaperInferencePrice(d.Pricing.CacheWrite),
			BaseURL:          baseURL,
			Source:           "live",
		})
	}
	return models, nil
}

func cheaperInferencePrice(raw string) float64 {
	price, err := strconv.ParseFloat(raw, 64)
	if err != nil || price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0
	}
	return price
}

package provider

import (
	"math"
	"testing"
)

func TestOpenCodeGoLongContextPricing(t *testing.T) {
	for _, tc := range []struct {
		model                      string
		threshold                  int
		input, output, read, write float64
	}{
		{"claude-haiku-5-5", 100000, 0.5, 2.5, 0.05, 0.625},
		{"gpt-6-luna", 272000, 0.2, 0.75, 0.02, 0.25},
		{"grok-4.7", 200000, 4, 12, 1, 0},
		{"qwen3.7-plus", 256000, 1.2, 4.8, 0.12, 1.5},
	} {
		t.Run(tc.model, func(t *testing.T) {
			model, err := FindModel("opencode-go", tc.model)
			if err != nil {
				t.Fatal(err)
			}
			if model.PriceTierInputTokens != tc.threshold {
				t.Fatalf("threshold = %d, want %d", model.PriceTierInputTokens, tc.threshold)
			}
			for _, above := range []bool{false, true} {
				usage := Usage{InputTokens: tc.threshold - 2000, CacheReadTokens: 1000, CacheWriteTokens: 1000, OutputTokens: 1000}
				input, output, read, write := model.PriceInput, model.PriceOutput, model.PriceCacheRead, model.PriceCacheWrite
				if above {
					usage.InputTokens++
					input, output, read, write = tc.input, tc.output, tc.read, tc.write
				}
				want := (float64(usage.InputTokens)*input + 1000*(output+read+write)) / 1000000
				if got := ComputeCost(model, usage); math.Abs(got-want) > 1e-10 {
					t.Fatalf("above=%v cost=%g, want %g", above, got, want)
				}
			}
		})
	}
}

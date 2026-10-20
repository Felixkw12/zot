package provider

import "testing"

func TestMiniMaxFlashDefaultRespectsReasoningOverrides(t *testing.T) {
	for _, tc := range []struct {
		name   string
		levels map[string]string
		want   string
	}{
		{"default", nil, "max"},
		{"removed max", map[string]string{"max": ""}, "xhigh"},
		{"remapped max", map[string]string{"max": "high"}, "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetLiveModels(nil)
			t.Cleanup(func() { SetLiveModels(nil) })
			SetUserModels([]Model{{Provider: "minimax", ID: "MiniMax-M3.1-Flash", Reasoning: true, ReasoningLevelMap: tc.levels, Source: "user"}})
			m, err := FindModel("minimax", "MiniMax-M3.1-Flash")
			if err != nil {
				t.Fatal(err)
			}
			anth := NewAnthropicCompat("minimax", "test-key", "http://localhost").(*anthropicClient)
			oai := NewOpenAICompat("minimax", "test-key", "http://localhost", "").(*openaiClient)
			for _, requested := range []string{"", "off", "max"} {
				if got := ClampReasoningForModel(m, requested); got != tc.want {
					t.Errorf("clamp %q = %q, want %q", requested, got, tc.want)
				}
				req := Request{Model: m.ID, Reasoning: requested}
				anthWire, err := anth.buildRequest(req)
				if err != nil {
					t.Fatal(err)
				}
				if anthWire.OutputConfig == nil || anthWire.OutputConfig.Effort != tc.want {
					t.Errorf("Anthropic effort for %q = %+v, want %q", requested, anthWire.OutputConfig, tc.want)
				}
				oaiWire, err := oai.buildRequest(req)
				if err != nil {
					t.Fatal(err)
				}
				if oaiWire.ReasoningEffort != tc.want {
					t.Errorf("OpenAI effort for %q = %q, want %q", requested, oaiWire.ReasoningEffort, tc.want)
				}
			}
		})
	}
}

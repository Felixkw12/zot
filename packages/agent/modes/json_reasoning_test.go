package modes

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type jsonReasoningClient struct{ wantReasoning bool }

func (jsonReasoningClient) Name() string { return "synthetic" }
func (c jsonReasoningClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	if req.StreamReasoning != c.wantReasoning {
		return nil, fmt.Errorf("reasoning requested=%v, want %v", req.StreamReasoning, c.wantReasoning)
	}
	out := make(chan provider.Event, 3)
	out <- provider.EventReasoningDelta{Delta: "thinking"}
	out <- provider.EventTextDelta{Delta: "answer"}
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "answer"}}}}
	close(out)
	return out, nil
}

func TestJSONReasoningRequiresOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var out bytes.Buffer
		ag := core.NewAgent(jsonReasoningClient{wantReasoning: enabled}, "test", "", nil)
		var err error
		if enabled {
			err = RunJSONWithOptions(context.Background(), ag, "hi", nil, &out, JSONOptions{ShowReasoning: true})
		} else {
			err = RunJSON(context.Background(), ag, "hi", nil, &out)
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), `"type":"reasoning_delta"`) != enabled || !strings.Contains(out.String(), `"type":"text_delta"`) {
			t.Fatalf("enabled=%v events=%s", enabled, &out)
		}
	}
}

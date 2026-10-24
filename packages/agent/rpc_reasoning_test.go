package agent

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type rpcReasoningClient struct{ wantReasoning bool }

func (rpcReasoningClient) Name() string { return "synthetic" }
func (c rpcReasoningClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
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

func TestRPCReasoningRequiresOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var out bytes.Buffer
		s := &rpcServer{ctx: context.Background(), args: Args{ShowReasoning: enabled}, agent: core.NewAgent(rpcReasoningClient{wantReasoning: enabled}, "test", "", nil), out: &out}
		s.runPrompt("1", "hi", nil)
		if strings.Contains(out.String(), `"type":"reasoning_delta"`) != enabled || !strings.Contains(out.String(), `"type":"text_delta"`) {
			t.Fatalf("enabled=%v events=%s", enabled, &out)
		}
	}
}

func TestParseArgsShowReasoning(t *testing.T) {
	for _, mode := range []string{"--json", "rpc"} {
		args, err := ParseArgs([]string{mode, "--show-reasoning"})
		if err != nil || !args.ShowReasoning {
			t.Fatalf("args=%+v, err=%v", args, err)
		}
	}
}

func TestRPCSetReasoningMax(t *testing.T) {
	var out bytes.Buffer
	s := &rpcServer{agent: &core.Agent{}, out: &out}
	s.dispatch("set_reasoning", "1", []byte(`{"reasoning":"max"}`))

	if s.agent.Reasoning != "max" {
		t.Fatalf("reasoning = %q, want max", s.agent.Reasoning)
	}
	if !strings.Contains(out.String(), `"reasoning":"max"`) {
		t.Fatalf("response = %q", out.String())
	}
}

func TestRPCSetReasoningRejectsUnknownLevel(t *testing.T) {
	var out bytes.Buffer
	s := &rpcServer{agent: &core.Agent{}, out: &out}
	s.dispatch("set_reasoning", "1", []byte(`{"reasoning":"extreme"}`))

	if s.agent.Reasoning != "" {
		t.Fatalf("reasoning = %q, want unchanged", s.agent.Reasoning)
	}
	if !strings.Contains(out.String(), `"success":false`) {
		t.Fatalf("response = %q", out.String())
	}
}

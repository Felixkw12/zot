package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestBeforeTurnContextPrecedenceAndLegacyFallback(t *testing.T) {
	client := &queueFakeClient{}
	ag := NewAgent(client, "model", "system", nil)
	legacyCalls, contextCalls := 0, 0
	ag.BeforeTurn = func(step int) (bool, string) {
		legacyCalls++
		return false, "legacy gate"
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag.BeforeTurnContext = func(turnCtx context.Context, step int) (bool, string) {
		contextCalls++
		if turnCtx != ctx || step != 1 {
			t.Fatalf("context hook received context=%v step=%d", turnCtx, step)
		}
		return false, "context gate"
	}
	if err := ag.Prompt(ctx, "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	if contextCalls != 1 || legacyCalls != 0 {
		t.Fatalf("context calls=%d legacy calls=%d", contextCalls, legacyCalls)
	}
	ag.BeforeTurnContext = nil
	if err := ag.Prompt(ctx, "hello again", nil, nil); err != nil {
		t.Fatal(err)
	}
	if legacyCalls != 1 || atomic.LoadInt32(&client.calls) != 0 {
		t.Fatalf("legacy calls=%d provider calls=%d", legacyCalls, client.calls)
	}
}

func TestBeforeTurnContextCancellationStopsBeforeProvider(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		client := &queueFakeClient{}
		ag := NewAgent(client, "model", "system", nil)
		ctx, cancel := context.WithCancel(context.Background())
		ag.BeforeTurnContext = func(turnCtx context.Context, step int) (bool, string) {
			cancel()
			if !errors.Is(turnCtx.Err(), context.Canceled) {
				t.Fatal("turn hook did not receive prompt cancellation")
			}
			return allowed, ""
		}
		starts, ends := 0, 0
		err := ag.Prompt(ctx, "hello", nil, func(event AgentEvent) {
			switch event := event.(type) {
			case EvTurnStart:
				starts++
			case EvTurnEnd:
				ends++
				if !errors.Is(event.Err, context.Canceled) {
					t.Fatalf("turn end error=%v", event.Err)
				}
			}
		})
		cancel()
		if starts != 1 || ends != 1 {
			t.Fatalf("unpaired turn events: starts=%d ends=%d", starts, ends)
		}
		if !errors.Is(err, context.Canceled) || atomic.LoadInt32(&client.calls) != 0 {
			t.Fatalf("allowed=%v error=%v provider calls=%d", allowed, err, client.calls)
		}
	}
}

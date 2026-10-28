package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type reasoningSettingsTestStore struct {
	SettingsStore
	enabled bool
	err     error
}

func (s *reasoningSettingsTestStore) SetShowReasoning(enabled bool) error {
	s.enabled = enabled
	return s.err
}

func TestLiveReasoningSetting(t *testing.T) {
	store := &reasoningSettingsTestStore{}
	i := NewInteractive(InteractiveConfig{SettingsStore: store, Terminal: &cleanupTestTerminal{}})
	if i.reasoningPreviewSetting().value {
		t.Fatal("reasoning preview must default to off")
	}
	i.handleEvent(core.EvReasoningDelta{Delta: "hidden"})
	if i.reasoning.String() != "" {
		t.Fatal("disabled preview accumulated reasoning")
	}
	i.applySettingToggle("show_reasoning", true)
	if !i.cfg.ShowReasoning || !store.enabled {
		t.Fatal("setting was not applied and persisted")
	}
	i.handleEvent(core.EvReasoningDelta{Delta: "live"})
	if i.reasoning.String() != "live" || i.streaming.Len() != 0 || len(i.streamPending) != 0 {
		t.Fatal("reasoning missing or mixed with answer pacing")
	}
	i.handleEvent(core.EvAssistantStart{})
	i.handleEvent(core.EvReasoningDelta{Delta: "visible reasoning"})
	if chat := strings.Join(i.cachedChatLocked(80), "\n"); !strings.Contains(chat, "visible reasoning") {
		t.Fatal("reasoning did not reach the interactive view")
	}
	store.err = errors.New("cannot save")
	i.applySettingToggle("show_reasoning", false)
	if !i.cfg.ShowReasoning || i.reasoning.String() != "visible reasoning" || i.statusErr == "" {
		t.Fatal("failed save changed live state or lost error")
	}
	store.err = nil
	i.applySettingToggle("show_reasoning", false)
	if i.cfg.ShowReasoning || i.reasoning.String() != "" {
		t.Fatal("disabled preview retained reasoning")
	}
	if chat := strings.Join(i.cachedChatLocked(80), "\n"); strings.Contains(chat, "reasoning (live)") {
		t.Fatal("disabled reasoning remained in the interactive view")
	}
}

func TestLiveReasoningRequestsProviderSummaries(t *testing.T) {
	ag := core.NewAgent(jsonReasoningClient{wantReasoning: true}, "test", "", nil)
	i := NewInteractive(InteractiveConfig{Agent: ag, ShowReasoning: true, Terminal: &cleanupTestTerminal{}})
	if err := ag.Prompt(context.Background(), "startup", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		ag.Client = jsonReasoningClient{wantReasoning: enabled}
		i.applySettingToggle("show_reasoning", enabled)
		if err := ag.Prompt(context.Background(), "after toggle", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLiveReasoningLifecycle(t *testing.T) {
	for _, event := range []core.AgentEvent{
		core.EvAssistantStart{},
		core.EvAssistantMessage{},
		core.EvTurnEnd{Stop: provider.StopEnd},
		core.EvTurnEnd{Stop: provider.StopAborted},
		core.EvTurnEnd{Stop: provider.StopError, Err: errors.New("failed")},
		core.EvDone{},
		core.EvError{},
	} {
		t.Run(event.Type(), func(t *testing.T) {
			i := NewInteractive(InteractiveConfig{ShowReasoning: true})
			i.handleEvent(core.EvAssistantStart{})
			i.handleEvent(core.EvReasoningDelta{Delta: "old reasoning"})
			i.handleEvent(event)
			if i.reasoning.String() != "" {
				t.Fatal("stale reasoning after lifecycle event")
			}
		})
	}
	i := NewInteractive(InteractiveConfig{ShowReasoning: true})
	i.reasoning.Append("old")
	i.resetStreamingStateLocked()
	if i.reasoning.String() != "" {
		t.Fatal("streaming reset retained reasoning")
	}
}

func TestReasoningSurvivesPacedCompletionAndRestore(t *testing.T) {
	ag := core.NewAgent(jsonReasoningClient{wantReasoning: true}, "test", "", nil)
	i := NewInteractive(InteractiveConfig{Agent: ag, ShowReasoning: true, Terminal: &cleanupTestTerminal{}})
	i.busy = true
	if err := ag.Prompt(context.Background(), "hi", nil, i.handleEvent); err != nil {
		t.Fatal(err)
	}
	if !i.streamFlushPending || i.reasoning.String() != "thinking" {
		t.Fatal("reasoning disappeared before the paced answer finished")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); i.runStreamPacer(ctx) }()
	defer func() { cancel(); <-done }()
	for {
		i.mu.Lock()
		finished := !i.streamFlushPending
		i.mu.Unlock()
		if finished {
			break
		}
		select {
		case <-i.dirty:
		case <-ctx.Done():
			t.Fatal("answer pacing did not finish", ctx.Err())
		}
	}
	i.mu.Lock()
	i.busy = false
	chat := strings.Join(i.cachedChatLocked(80), "\n")
	live := i.reasoning.String()
	i.mu.Unlock()
	if live != "" || strings.Count(chat, "thinking") != 1 || strings.Index(chat, "thinking") > strings.Index(chat, "answer") {
		t.Fatalf("reasoning was lost, duplicated, or placed after the answer: %q", chat)
	}
	i.applySettingToggle("show_reasoning", false)
	if chat := strings.Join(i.cachedChatLocked(80), "\n"); strings.Contains(chat, "thinking") {
		t.Fatal("disabled setting retained saved reasoning")
	}
	i.applySettingToggle("show_reasoning", true)
	if chat := strings.Join(i.cachedChatLocked(80), "\n"); !strings.Contains(chat, "thinking") {
		t.Fatal("enabling the setting did not restore saved reasoning")
	}
	restored := NewInteractive(InteractiveConfig{Agent: ag, ShowReasoning: true, Terminal: &cleanupTestTerminal{}})
	if chat := strings.Join(restored.cachedChatLocked(80), "\n"); !strings.Contains(chat, "thinking") || strings.Contains(chat, "reasoning (live)") {
		t.Fatal("new view did not display saved reasoning")
	}
}

func TestReasoningDeltaJSON(t *testing.T) {
	got := EventToJSON(core.EvReasoningDelta{Delta: "readable"})
	if got["type"] != "reasoning_delta" || got["delta"] != "readable" || len(got) != 2 {
		t.Fatalf("reasoning event = %#v", got)
	}
}

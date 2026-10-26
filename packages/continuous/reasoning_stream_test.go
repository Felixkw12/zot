package continuous

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/patriceckhart/zot/packages/core"
)

func TestPartialStreamReasoningOverlapAndAttemptReset(t *testing.T) {
	var stream partialStream
	var got []string
	sink := func(event core.AgentEvent) {
		switch e := event.(type) {
		case core.EvAssistantStart:
			got = append(got, "start")
		case core.EvReasoningDelta:
			got = append(got, "reasoning:"+e.Delta)
		case core.EvTextDelta:
			got = append(got, "text:"+e.Delta)
		}
	}
	p := Partial{RunID: "r", Turn: 1, Attempt: 1, Reasoning: "first", ReasoningBytes: 5}
	stream.apply(p, sink)
	stream.apply(p, sink) // Repeated commits must not duplicate a suffix.
	p.Reasoning, p.ReasoningBytes = "st second", 12
	stream.apply(p, sink)
	p.Text = "answer"
	stream.apply(p, sink)                         // Do not restart the preview when answer text starts.
	p.Reasoning, p.ReasoningBytes = "latest", 100 // A skipped prefix after a watch gap.
	stream.apply(p, sink)
	p.Attempt++
	p.Reasoning, p.ReasoningBytes = "retry", 5
	stream.apply(p, sink) // Identical answer prefixes still belong to a new attempt.
	p.Final = true
	p.Text = "ignored"
	stream.apply(p, sink)
	want := []string{"start", "reasoning:first", "reasoning: second", "text:answer", "reasoning:latest", "start", "reasoning:retry", "text:answer"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%q, want %q", got, want)
	}
	stream.reset()
	p.Final, p.Text = false, ""
	stream.apply(p, sink) // Reattach during reasoning only.
	if got[len(got)-2] != "start" || got[len(got)-1] != "reasoning:retry" {
		t.Fatalf("restored events=%q", got)
	}
}

func TestTaskPartialReasoningKeepsBoundedUTF8Tail(t *testing.T) {
	gate := newEventGate(nil)
	p := &taskPartial{gate: gate}
	large := strings.Repeat("古", MaxPartialReasoningBytes)
	p.addReasoning(large, core.EvReasoningDelta{Delta: large})
	p.addReasoning("latest", core.EvReasoningDelta{Delta: "latest"})
	if len(p.reasoning) > MaxPartialReasoningBytes || !utf8.ValidString(p.reasoning) || !strings.HasSuffix(p.reasoning, "latest") || p.reasoningBytes != int64(len(large)+6) {
		t.Fatal("invalid bounded reasoning suffix")
	}
	if text, truncated := p.text(); text != "" || truncated {
		t.Fatal("reasoning contaminated answer text")
	}
	if !p.dirty {
		t.Fatal("reasoning-only stream did not schedule a commit")
	}
}

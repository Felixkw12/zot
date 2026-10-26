package tui

import (
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestSavedReasoningRemainsBeforeAnswer(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		msg := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ReasoningBlock{Summary: "older summary", Encrypted: "private-signature"},
			provider.TextBlock{Text: "final answer"},
		}}
		if !legacy {
			msg.Meta = map[string]string{provider.ReadableReasoningMetaKey: "old\none\ntwo\nlatest reasoning"}
		}
		v := &View{Theme: Dark, Messages: []provider.Message{msg}}
		build := func() string { return stripANSI(strings.Join(v.Build(80), "\n")) }
		if got := build(); strings.Contains(got, "reasoning") || strings.Contains(got, "older summary") {
			t.Fatalf("default view revealed reasoning: %q", got)
		}
		v.ShowReasoning = true
		want := "latest reasoning"
		if legacy {
			want = "older summary"
		}
		for n := 0; n < 2; n++ {
			got := build()
			if strings.Index(got, want) < 0 || strings.Index(got, want) > strings.Index(got, "final answer") {
				t.Fatalf("saved reasoning missing or out of order: %q", got)
			}
			if strings.Contains(got, "(live)") || strings.Contains(got, "private-signature") {
				t.Fatalf("saved view leaked opaque data or used live label: %q", got)
			}
			if !legacy && strings.Contains(got, "older summary") {
				t.Fatal("replay summary duplicated the display tail")
			}
		}
		v.ShowReasoning = false
		if strings.Contains(build(), want) {
			t.Fatal("cached reasoning survived disabling the display")
		}
		v.ShowReasoning = true
		if !strings.Contains(build(), want) {
			t.Fatal("enabling the display did not restore saved reasoning")
		}
	}
}

func TestSavedReasoningCacheTracksMetadata(t *testing.T) {
	v := &View{Theme: Dark, ShowReasoning: true, Messages: []provider.Message{{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "answer"}},
		Meta:    map[string]string{provider.ReadableReasoningMetaKey: "first reasoning"},
	}}}
	v.Build(80)
	v.Messages[0].Meta[provider.ReadableReasoningMetaKey] = "updated reasoning"
	got := stripANSI(strings.Join(v.Build(80), "\n"))
	if !strings.Contains(got, "updated reasoning") || strings.Contains(got, "first reasoning") {
		t.Fatalf("stale reasoning render: %q", got)
	}
}

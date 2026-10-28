package tui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReasoningPreviewBoundedSuffix(t *testing.T) {
	var p ReasoningPreview
	p.Append(strings.Repeat("古", 3*reasoningPreviewRunes))
	p.Append("latest")
	if got := p.String(); !utf8.ValidString(got) || len([]rune(got)) != reasoningPreviewRunes || !strings.HasSuffix(got, "latest") {
		t.Fatalf("invalid tail: bytes=%d suffix=%q", len(got), got[len(got)-6:])
	}
	p.Reset()
	if p.String() != "" {
		t.Fatal("reset retained reasoning")
	}
}

func TestRenderReasoningPreviewTail(t *testing.T) {
	lines := RenderReasoningPreview("old\nolder\none\ntwo\nlatest\n", Dark, 40)
	plain := stripANSI(strings.Join(lines, "\n"))
	if strings.Contains(plain, "old") || !strings.Contains(plain, "one\n") || !strings.Contains(plain, "latest") || len(lines) != 5 {
		t.Fatalf("unexpected preview: %q", plain)
	}
	// Wrap before taking the tail, including a partial last line.
	lines = RenderReasoningPreview("aaaabbbbccccdddde", Dark, 6)
	plain = stripANSI(strings.Join(lines, "\n"))
	if strings.Contains(plain, "aaaa") || strings.Contains(plain, "bbbb") || !strings.Contains(plain, "cccc") || !strings.Contains(plain, "dddd") || !strings.Contains(plain, "  e") {
		t.Fatalf("unexpected wrapped tail: %q", plain)
	}
}

func TestRenderReasoningPreviewWidthsAndControls(t *testing.T) {
	for _, width := range []int{0, 1, 2, 3, 4, 8, 80} {
		lines := RenderReasoningPreview("古ä\ttext\x1b[2J\x1b]52;c;secret\a\r\nlatest", Dark, width)
		for _, line := range lines {
			plain := stripANSI(line)
			if visibleWidth(line) > width || strings.ContainsAny(plain, "\x1b\a\r\t") {
				t.Errorf("width %d, invalid line %q", width, line)
			}
		}
	}
	if got := RenderReasoningPreview(" \n\t", Dark, 80); len(got) != 0 {
		t.Fatalf("blank preview = %q", got)
	}
}

func TestViewReasoningLiveBufferIsSeparate(t *testing.T) {
	v := &View{Theme: Dark, Reasoning: "latest reasoning"}
	for _, lines := range [][]string{v.Build(80), v.BuildLive(80)} {
		if !strings.Contains(stripANSI(strings.Join(lines, "\n")), "latest reasoning") {
			t.Fatal("live reasoning missing")
		}
	}
	if len(v.Messages) != 0 {
		t.Fatal("reasoning became a message")
	}
	v.Reasoning = ""
	if strings.Contains(stripANSI(strings.Join(v.Build(80), "\n")), "latest reasoning") {
		t.Fatal("stale cached reasoning")
	}
}

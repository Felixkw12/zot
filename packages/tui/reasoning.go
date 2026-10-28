package tui

import (
	"strings"
	"unicode"
)

const reasoningPreviewRunes = 8192
const reasoningPreviewLines = 3

// ReasoningPreview retains a bounded suffix, not a second transcript.
// Its zero value is ready to use. The caller must synchronize access.
type ReasoningPreview struct {
	tail []rune
}

func (p *ReasoningPreview) Append(delta string) {
	for _, r := range delta {
		p.tail = append(p.tail, r)
		if len(p.tail) == 2*reasoningPreviewRunes {
			copy(p.tail, p.tail[reasoningPreviewRunes:])
			p.tail = p.tail[:reasoningPreviewRunes]
		}
	}
	if len(p.tail) > reasoningPreviewRunes {
		copy(p.tail, p.tail[len(p.tail)-reasoningPreviewRunes:])
		p.tail = p.tail[:reasoningPreviewRunes]
	}
}

func (p *ReasoningPreview) Reset()         { p.tail = nil }
func (p *ReasoningPreview) String() string { return string(p.tail) }

// RenderReasoningPreview shows the last three wrapped lines of plain text.
// Model-supplied terminal controls are discarded rather than interpreted.
func RenderReasoningPreview(text string, th Theme, width int) []string {
	return renderReasoningTail(text, "reasoning (live)", th, width)
}

func renderReasoningSummary(text string, th Theme, width int) []string {
	var tail ReasoningPreview
	tail.Append(text)
	return renderReasoningTail(tail.String(), "reasoning", th, width)
}

func renderReasoningTail(text, label string, th Theme, width int) []string {
	if width < 1 {
		return nil
	}
	text = strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	text = strings.TrimRight(text, " \n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	indent := "  "
	if width < 4 {
		indent = ""
	}
	inner := width - len(indent)
	var body []string
	for _, line := range strings.Split(text, "\n") {
		body = append(body, wrapANSILine(line, inner)...)
	}
	if len(body) > reasoningPreviewLines {
		body = body[len(body)-reasoningPreviewLines:]
	}
	header := truncateToWidth(label, inner)
	out := []string{indent + th.FG256(th.Muted, header)}
	for _, line := range body {
		out = append(out, indent+th.FG256(th.Muted, truncateToWidth(line, inner)))
	}
	return append(out, "")
}

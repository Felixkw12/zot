package tui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestImagePreviewProtocolsAndFootprint(t *testing.T) {
	t.Setenv("ZOT_CELL_ASPECT", "2")
	b := provider.ImageBlock{MimeType: "image/png", Data: testPNG(t, 40, 100)}
	for _, proto := range []ImageProtocol{ImageProtocolNone, ImageProtocolKitty, ImageProtocolITerm2} {
		v := View{Theme: Dark, ImageProto: proto}
		lines := v.RenderImagePreview(b, 30, 4)
		joined := strings.Join(lines, "\n")
		if strings.Contains(joined, imageFootprintSentinel) {
			t.Fatal("preview leaked footprint sentinel")
		}
		if !strings.Contains(joined, "40x100") {
			t.Fatal("missing dimensions")
		}
		if proto == ImageProtocolNone {
			if len(lines) != 1 || hasImageEscapeLine(joined) {
				t.Fatal("fallback rendered graphics")
			}
		} else if len(lines) != 6 || !hasImageEscapeLine(lines[0]) {
			t.Fatalf("protocol %v: expected four image rows, gap and caption, got %d", proto, len(lines))
		}
	}
}

func TestImagePreviewNarrowAndEmptyLayouts(t *testing.T) {
	v := View{Theme: Dark, ImageProto: ImageProtocolKitty}
	b := provider.ImageBlock{MimeType: "image/png", Data: testPNG(t, 100, 20)}
	for _, width := range []int{0, 1, 4, 5, 8} {
		lines := v.RenderImagePreview(b, width, 3)
		for _, line := range lines {
			if !hasImageEscapeLine(line) && visibleWidth(line) > width {
				t.Fatalf("width %d: row overflows", width)
			}
		}
		if width <= 4 && hasImageEscapeLine(strings.Join(lines, "")) {
			t.Fatal("graphics cannot fit indent")
		}
	}
	if got := v.RenderImagePreview(b, 80, 0); len(got) != 0 {
		t.Fatal("zero height should render nothing")
	}
}

func TestUserImageMessagesRenderThumbnails(t *testing.T) {
	b := provider.ImageBlock{MimeType: "image/png", Data: testPNG(t, 40, 30)}
	for _, compact := range []bool{false, true} {
		for _, proto := range []ImageProtocol{ImageProtocolNone, ImageProtocolKitty} {
			v := View{Theme: Dark, CompactUser: compact, ImageProto: proto,
				Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{b, b}}}}
			lines := v.Build(80)
			joined := strings.Join(lines, "\n")
			if strings.Contains(joined, imageFootprintSentinel) || strings.Count(joined, "40x30") != 2 {
				t.Fatal("user attachments lost metadata or leaked sentinel")
			}
			if proto != ImageProtocolNone && strings.Count(joined, "\x1b_G") != 2 {
				t.Fatal("both user images should render")
			}
		}
	}
}

func TestUserImageDoesNotPaintFullWidthBubble(t *testing.T) {
	t.Setenv("ZOT_CELL_ASPECT", "2")
	b := provider.ImageBlock{MimeType: "image/png", Data: testPNG(t, 226, 147)}
	v := View{Theme: Dark, ImageProto: ImageProtocolKitty}
	lines := v.renderMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{b}}, 100, false)
	if strings.Contains(strings.Join(lines, "\n"), "[48;") {
		t.Fatal("image-only messages must not have full-width bubble backgrounds")
	}
	if len(lines) < 10 {
		t.Fatal("sent screenshot thumbnail is too small")
	}

	lines = v.renderMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{
		provider.TextBlock{Text: "describe this"}, b,
	}}, 100, false)
	if got := userBubbleRows(stripANSI(strings.Join(lines, "\n"))); got != 3 {
		t.Fatalf("only the text should use a padded bubble, got %d gutter rows", got)
	}
}

func TestDrawLogImagePreviewRepaintAndRemoval(t *testing.T) {
	var out bytes.Buffer
	r := NewRenderer(&out)
	r.Resize(80, 24)
	bottom := []string{"\x1b_Gf=100;AAAA\x1b\\", "", "image - png", "editor"}
	chat := append([]string{"old scrollback"}, make([]string, 40)...)
	r.DrawLog(chat, bottom, 3, 2)
	out.Reset()
	chat[len(chat)-1] = "changed chat"
	r.DrawLog(chat, bottom, 3, 2)
	if strings.Contains(out.String(), "old scrollback") {
		t.Fatal("thumbnail repaint replayed old transcript into scrollback")
	}
	if !strings.Contains(out.String(), SeqDeleteKittyImages) || !strings.Contains(out.String(), bottom[0]) {
		t.Fatal("changed frame must replace graphics")
	}
	out.Reset()
	r.DrawLog(chat, bottom, 3, 2)
	if out.Len() != 0 {
		t.Fatal("idle thumbnail should not repaint")
	}
	out.Reset()
	r.DrawLog(chat, []string{"editor"}, 0, 2)
	if !strings.Contains(out.String(), SeqDeleteKittyImages) {
		t.Fatal("removed preview must delete stale graphics")
	}
}

package modes

import (
	"fmt"
	"os"
	"strings"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

type clipboardImageAttachment struct {
	Marker string
	Image  provider.ImageBlock
}

// clipboardImagePreviews only shows attachments whose markers remain in the
// editor. Reserve space for a summary if the terminal cannot fit every image.
func clipboardImagePreviews(view *tui.View, text string, pending []clipboardImageAttachment, width, budget int) []string {
	if budget < 1 || width < 1 {
		return nil
	}
	var active []clipboardImageAttachment
	for _, item := range pending {
		if item.Marker != "" && strings.Contains(text, item.Marker) {
			active = append(active, item)
		}
	}
	var lines []string
	for idx, item := range active {
		remaining := budget - len(lines)
		if remaining < 5 {
			lines = append(lines, truncateLine(fmt.Sprintf("  %d image previews hidden (enlarge terminal)", len(active)-idx), width))
			break
		}
		lines = append(lines, view.Theme.FG256(view.Theme.Muted, truncateLine("  "+item.Marker, width)))
		lines = append(lines, view.RenderImagePreview(item.Image, width, min(4, remaining-4))...)
	}
	return lines
}

type clipboardImageReader func() (string, []byte, bool, error)

func readClipboardImageAndCleanup(read clipboardImageReader) ([]byte, bool, error) {
	path, data, ok, err := read()
	if path != "" {
		if removeErr := os.Remove(path); removeErr != nil && err == nil {
			err = fmt.Errorf("remove temporary clipboard image: %w", removeErr)
		}
	}
	return data, ok, err
}

func preparePromptWithClipboardImages(text string, pending []clipboardImageAttachment) (string, []provider.ImageBlock) {
	if len(pending) == 0 {
		return text, nil
	}

	out := text
	images := make([]provider.ImageBlock, 0, len(pending))
	for _, item := range pending {
		if item.Marker == "" || !strings.Contains(out, item.Marker) {
			continue
		}
		out = removeClipboardMarker(out, item.Marker)
		images = append(images, item.Image)
	}
	return strings.TrimSpace(out), images
}

func removeClipboardMarker(text, marker string) string {
	for {
		idx := strings.Index(text, marker)
		if idx < 0 {
			return text
		}
		end := idx + len(marker)
		prevInline := idx > 0 && isInlineWhitespace(text[idx-1])
		nextInline := end < len(text) && isInlineWhitespace(text[end])
		prevLineBreak := idx == 0 || idx > 0 && isLineBreak(text[idx-1])
		nextLineBreak := end == len(text) || end < len(text) && isLineBreak(text[end])
		switch {
		case prevInline && nextInline:
			text = text[:idx] + text[end+1:]
		case prevInline && nextLineBreak:
			text = text[:idx-1] + text[end:]
		case prevLineBreak && nextInline:
			text = text[:idx] + text[end+1:]
		default:
			text = text[:idx] + text[end:]
		}
	}
}

func isInlineWhitespace(b byte) bool {
	return b == ' ' || b == '\t'
}

func isLineBreak(b byte) bool {
	return b == '\n' || b == '\r'
}

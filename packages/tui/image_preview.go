package tui

import "github.com/patriceckhart/zot/packages/provider"

// RenderImagePreview returns a thumbnail with reserved blank rows and a metadata
// caption. maxRows bounds the graphics height, excluding the gap and caption.
// Unsupported terminals and very narrow layouts use a text fallback.
func (v *View) RenderImagePreview(b provider.ImageBlock, width, maxRows int) []string {
	if width < 1 || maxRows < 1 {
		return nil
	}
	lines := v.renderImageBlockSized(b, min(width, 52), maxRows)
	for idx, line := range lines {
		_, line = parseImageFootprint(line)
		lines[idx] = truncateToWidth(line, width)
	}
	return lines
}

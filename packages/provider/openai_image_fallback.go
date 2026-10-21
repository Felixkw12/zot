package provider

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"strings"

	_ "golang.org/x/image/webp"
)

// retryImagesAsJPEG changes only the disposable wire request, not the source
// messages. This also handles images replayed from existing sessions and images
// returned by tools without weakening the normal MIME/content pairing.
func (wire *oaiRequest) retryImagesAsJPEG() bool {
	changed := false
	for i := range wire.Messages {
		parts, ok := wire.Messages[i].Content.([]interface{})
		if !ok {
			continue
		}
		for j, part := range parts {
			block, ok := part.(oaiContentImage)
			if !ok {
				continue
			}
			url, ok := openAIImageJPEGFallback(block.ImageURL.URL)
			if !ok {
				continue
			}
			block.ImageURL.URL = url
			parts[j] = block
			changed = true
		}
	}
	return changed
}

// Some local OpenAI-compatible APIs reject PNG/WebP data URLs with a misleading
// base64 validation error. Re-encode rather than lying about the MIME or
// stripping the standard data URL prefix. JPEG is lossy and has no alpha, so
// this is only used after an explicit rejection, with transparency on white.
func openAIImageJPEGFallback(url string) (string, bool) {
	const maxBytes = 32 << 20
	const maxPixels = 25_000_000
	var payload string
	switch {
	case strings.HasPrefix(url, "data:image/png;base64,"):
		payload = strings.TrimPrefix(url, "data:image/png;base64,")
	case strings.HasPrefix(url, "data:image/webp;base64,"):
		payload = strings.TrimPrefix(url, "data:image/webp;base64,")
	default:
		return "", false
	}
	if len(payload) > base64.StdEncoding.EncodedLen(maxBytes) {
		return "", false
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(data) > maxBytes {
		return "", false
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxPixels/cfg.Height {
		return "", false
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", false
	}
	dst := image.NewRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Over)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 90}); err != nil {
		return "", false
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), true
}

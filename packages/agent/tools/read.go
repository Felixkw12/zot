// Package tools implements zot's built-in tools: read, write, edit, bash, glob.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
)

const (
	maxReadLines = 2000
	maxReadBytes = 50 * 1024
)

// ReadTool reads file contents from disk.
type ReadTool struct {
	CWD     string
	Sandbox *Sandbox // when jailed, confines reads to the sandbox root
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

const readSchema = `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["path"]}`

func (t *ReadTool) Name() string { return "read" }
func (t *ReadTool) Description() string {
	return "Read a file. Images (png/jpg/gif/webp/bmp/tiff) return inline; BMP and TIFF are converted to PNG."
}
func (t *ReadTool) Schema() json.RawMessage { return json.RawMessage(readSchema) }

// ReplayPolicy declares reads safe to repeat after an interrupted durable run.
// Reading has no external effect; a repeat returns current file contents.
func (t *ReadTool) ReplayPolicy() core.ToolReplayPolicy { return core.ReplaySafe }

func (t *ReadTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	var a readArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return core.ToolResult{}, fmt.Errorf("invalid args: %w", err)
	}
	if a.Path == "" {
		return core.ToolResult{}, fmt.Errorf("path is required")
	}
	path := resolvePath(t.CWD, a.Path)
	if err := t.Sandbox.CheckReadPath(path); err != nil {
		return core.ToolResult{}, err
	}
	// What the model sees in errors: relative to the sandbox root when
	// jailed, so absolute paths stay out of the context window.
	shown := t.Sandbox.DisplayPath(path, a.Path)

	info, err := os.Stat(path)
	if err != nil {
		return core.ToolResult{}, err
	}
	if info.IsDir() {
		return core.ToolResult{}, fmt.Errorf("%s is a directory", shown)
	}

	// Image handling. BMP and TIFF are decoded to PNG because vision
	// providers generally accept PNG but not either original format.
	if mime := imageMIME(path); mime != "" || convertibleImage(path) {
		data, err := os.ReadFile(path)
		if err != nil {
			return core.ToolResult{}, err
		}
		// The extension is only a hint. Files are routinely mislabeled
		// (a .png that is actually JPEG bytes, a renamed download, an
		// editor that re-encoded on save). Anthropic and other providers
		// sniff the real bytes and reject the whole request when the
		// declared media type disagrees, which would break the session.
		// Always derive the MIME from recognized content and reject
		// unrecognized bytes rather than sending an invalid image upstream.
		if sniffed := sniffImageMIME(data); sniffed != "" {
			mime = sniffed
		} else if convertibleImage(path) {
			const maxConvertBytes = 32 << 20
			const maxConvertPixels = 25_000_000
			if len(data) > maxConvertBytes {
				return core.ToolResult{}, fmt.Errorf("image %s is too large to convert", shown)
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				return core.ToolResult{}, fmt.Errorf("decode image %s: %w", shown, err)
			}
			if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxConvertPixels/cfg.Height {
				return core.ToolResult{}, fmt.Errorf("image %s is too large to convert", shown)
			}
			img, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				return core.ToolResult{}, fmt.Errorf("decode image %s: %w", shown, err)
			}
			var buf bytes.Buffer
			if err := png.Encode(&buf, img); err != nil {
				return core.ToolResult{}, fmt.Errorf("convert image %s to PNG: %w", shown, err)
			}
			data, mime = buf.Bytes(), "image/png"
		} else {
			return core.ToolResult{}, fmt.Errorf("%s is not a supported PNG, JPEG, GIF, or WebP image", shown)
		}
		return core.ToolResult{
			Content: []provider.Content{provider.ImageBlock{MimeType: mime, Data: data}},
		}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return core.ToolResult{}, err
	}
	defer f.Close()

	// Read up to maxReadBytes and also line-limit.
	limited := io.LimitReader(f, int64(maxReadBytes)+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return core.ToolResult{}, err
	}
	truncBytes := len(data) > maxReadBytes
	if truncBytes {
		data = data[:maxReadBytes]
	}

	// Recognize image formats zot cannot decode before the binary guard so
	// the error names the format instead of reporting a text-file problem.
	binary := looksBinary(data)
	if format := unsupportedImageFormat(path, data, binary); format != "" {
		return core.ToolResult{}, fmt.Errorf("%s is a %s image, which is not supported; convert it to PNG or JPEG first", a.Path, format)
	}
	if binary {
		return core.ToolResult{}, fmt.Errorf("%s looks binary; refusing to read as text", a.Path)
	}

	lines := strings.Split(string(data), "\n")
	// Trim trailing empty line from final \n.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	start := 0
	if a.Offset > 0 {
		start = a.Offset - 1
		if start > len(lines) {
			start = len(lines)
		}
	}
	end := len(lines)
	if a.Limit > 0 && start+a.Limit < end {
		end = start + a.Limit
	}
	selected := lines[start:end]

	truncLines := false
	if len(selected) > maxReadLines {
		selected = selected[:maxReadLines]
		truncLines = true
	}

	// Raw file contents go to the model. We deliberately DON'T
	// prepend line numbers here: they'd inflate the token count by
	// ~15-20% on typical source files (7 bytes per line, every
	// line, every time the file gets re-sent as context on later
	// turns) and the model doesn't need them — edit goes through
	// exact-match text replacement, not line ranges.
	//
	// The TUI renders its own gutter using the start offset stored
	// in Details, so the on-screen view still looks like cat -n.
	var sb strings.Builder
	for _, line := range selected {
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	if truncLines || truncBytes {
		sb.WriteString("\n")
	}
	if truncLines {
		sb.WriteString(fmt.Sprintf("... [truncated at %d lines]\n", maxReadLines))
	}
	if truncBytes {
		sb.WriteString(fmt.Sprintf("... [truncated at %d bytes]\n", maxReadBytes))
	}

	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: sb.String()}},
		Details: map[string]any{
			"path":            path,
			"start_line":      start + 1, // 1-indexed; TUI draws the gutter
			"lines_truncated": truncLines,
			"bytes_truncated": truncBytes,
			"total_lines":     len(lines),
		},
	}, nil
}

func resolvePath(cwd, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return filepath.Join(cwd, p)
}

func convertibleImage(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".bmp", ".tif", ".tiff":
		return true
	}
	return false
}

func imageMIME(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return ""
}

// sniffImageMIME inspects the leading bytes of an image file and
// returns the real media type, independent of the file's extension.
// Providers validate the declared media type against the actual bytes
// and 400 the whole request on a mismatch, so the extension can never
// be trusted. Returns "" when the format is not one zot ships images
// for, leaving the caller's extension-based guess in place.
func sniffImageMIME(data []byte) string {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}

// unsupportedImageFormat names image formats the read tool cannot decode,
// detected from content signatures first. The extension is only a fallback
// for binary content, so a text file with an image extension still reads.
// Returns "" for anything else so regular text and binary handling applies.
func unsupportedImageFormat(path string, data []byte, binary bool) string {
	if format := sniffUnsupportedImage(data); format != "" {
		return format
	}
	if !binary {
		return ""
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".heic", ".heics":
		return "HEIC"
	case ".heif", ".heifs", ".hif":
		return "HEIF"
	case ".avif", ".avifs":
		return "AVIF"
	case ".jp2", ".j2k", ".j2c", ".jpf", ".jpx", ".jpm":
		return "JPEG 2000"
	case ".jxl":
		return "JPEG XL"
	}
	return ""
}

func sniffUnsupportedImage(data []byte) string {
	switch {
	case len(data) >= 12 && bytes.Equal(data[:12], []byte{0, 0, 0, 0x0C, 'j', 'P', ' ', ' ', 0x0D, 0x0A, 0x87, 0x0A}),
		len(data) >= 4 && bytes.Equal(data[:4], []byte{0xFF, 0x4F, 0xFF, 0x51}):
		return "JPEG 2000"
	case len(data) >= 12 && bytes.Equal(data[:12], []byte{0, 0, 0, 0x0C, 'J', 'X', 'L', ' ', 0x0D, 0x0A, 0x87, 0x0A}),
		len(data) >= 2 && data[0] == 0xFF && data[1] == 0x0A:
		return "JPEG XL"
	}
	// HEIF family: an ISO BMFF ftyp box listing a HEIF or AVIF brand.
	if len(data) < 16 || !bytes.Equal(data[4:8], []byte("ftyp")) {
		return ""
	}
	size := int(data[0])<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	if size < 16 || size > len(data) {
		size = len(data)
	}
	format := ""
	for i := 8; i+4 <= size; i += 4 {
		if i == 12 {
			continue // minor version, not a brand
		}
		switch string(data[i : i+4]) {
		case "avif", "avis":
			return "AVIF"
		case "heic", "heix", "heim", "heis", "hevc", "hevx", "hevm", "hevs":
			format = "HEIC"
		case "mif1", "msf1":
			if format == "" {
				format = "HEIF"
			}
		}
	}
	return format
}

// looksBinary returns true if the buffer contains a NUL byte in its first 8 KiB.
func looksBinary(b []byte) bool {
	n := len(b)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

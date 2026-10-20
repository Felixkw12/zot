//go:build linux

package tui

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

type linuxImageClipboardBackend struct {
	name     string
	listArgs []string
	readArgs func(string) []string
}

// ReadClipboardImagePNG reads and normalizes an image from a Wayland or X11
// clipboard. Small command-line clients preserve zot's CGO-free build.
func ReadClipboardImagePNG() (string, []byte, bool, error) {
	backends := linuxImageClipboardBackends()
	var backendErrors []error
	for _, backend := range backends {
		data, ok, err := readLinuxClipboardImage(backend)
		if errors.Is(err, errClipboardImageCommandUnavailable) {
			continue
		}
		if err != nil {
			backendErrors = append(backendErrors, err)
			continue
		}
		if !ok {
			return "", nil, false, nil
		}
		pngData, err := normalizeClipboardImagePNG(data)
		if err != nil {
			return "", nil, false, fmt.Errorf("invalid clipboard image: %w", err)
		}
		return "", pngData, true, nil
	}
	if err := errors.Join(backendErrors...); err != nil {
		return "", nil, false, fmt.Errorf("read image clipboard: %w", err)
	}
	return "", nil, false, nil
}

func linuxImageClipboardBackends() []linuxImageClipboardBackend {
	wayland := linuxImageClipboardBackend{
		name:     "wl-paste",
		listArgs: []string{"--list-types"},
		readArgs: func(mime string) []string { return []string{"--no-newline", "--type", mime} },
	}
	xclip := linuxImageClipboardBackend{
		name:     "xclip",
		listArgs: []string{"-selection", "clipboard", "-target", "TARGETS", "-out"},
		readArgs: func(mime string) []string { return []string{"-selection", "clipboard", "-target", mime, "-out"} },
	}

	var backends []linuxImageClipboardBackend
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		backends = append(backends, wayland)
	}
	if os.Getenv("DISPLAY") != "" {
		backends = append(backends, xclip)
	}
	if len(backends) == 0 {
		backends = []linuxImageClipboardBackend{wayland, xclip}
	}
	return backends
}

func readLinuxClipboardImage(backend linuxImageClipboardBackend) ([]byte, bool, error) {
	types, err := runClipboardImageCommand(backend.name, maxClipboardMetadataBytes, backend.listArgs...)
	if err != nil {
		return nil, false, err
	}
	mime, ok := preferredClipboardImageMIME(string(types))
	if !ok {
		return nil, false, nil
	}
	data, err := runClipboardImageCommand(backend.name, maxClipboardImageBytes, backend.readArgs(mime)...)
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return nil, false, fmt.Errorf("%s returned empty data for %s", backend.name, mime)
	}
	return data, true, nil
}

func preferredClipboardImageMIME(types string) (string, bool) {
	available := make(map[string]string)
	for _, field := range strings.Fields(types) {
		mime := strings.TrimSpace(field)
		available[strings.ToLower(mime)] = mime
	}
	for _, mime := range []string{
		"image/png",
		"image/tiff",
		"image/x-tiff",
		"image/jpeg",
		"image/jpg",
		"image/webp",
		"image/gif",
	} {
		if original, ok := available[mime]; ok {
			return original, true
		}
	}
	return "", false
}

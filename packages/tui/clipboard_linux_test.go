//go:build linux

package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeLinuxClipboard(t *testing.T, name, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(path))
}

func TestReadClipboardImagePNGWayland(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "image.png")
	want := testClipboardPNG(t)
	if err := os.WriteFile(fixture, want, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIPBOARD_IMAGE_FIXTURE", fixture)
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("DISPLAY", "")
	writeFakeLinuxClipboard(t, "wl-paste", `
if [ "$1" = "--list-types" ]; then
    printf 'text/plain\nimage/png\n'
else
    /bin/cat "$CLIPBOARD_IMAGE_FIXTURE"
fi
`)

	path, got, ok, err := ReadClipboardImagePNG()
	if err != nil {
		t.Fatal(err)
	}
	if path != "" || !ok {
		t.Fatalf("path = %q, ok = %v", path, ok)
	}
	if !bytes.Equal(got, want) {
		// Normalization may choose a different valid PNG representation, so
		// compare dimensions when the encoded bytes differ.
		if gotW, gotH := ImageDimensions(got); gotW != 2 || gotH != 2 {
			t.Fatalf("normalized image dimensions = %dx%d", gotW, gotH)
		}
	}
}

func TestReadClipboardImagePNGX11(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(fixture, testClipboardPNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIPBOARD_IMAGE_FIXTURE", fixture)
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", ":1")
	writeFakeLinuxClipboard(t, "xclip", `
if [ "$4" = "TARGETS" ]; then
    printf 'TARGETS\nimage/png\n'
else
    /bin/cat "$CLIPBOARD_IMAGE_FIXTURE"
fi
`)

	_, got, ok, err := ReadClipboardImagePNG()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(got) == 0 {
		t.Fatalf("ok = %v, len(data) = %d", ok, len(got))
	}
}

func TestReadClipboardImagePNGNoImageFallsBack(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("DISPLAY", "")
	writeFakeLinuxClipboard(t, "wl-paste", `
if [ "$1" = "--list-types" ]; then
    printf 'text/plain;charset=utf-8\n'
    exit 0
fi
exit 9
`)

	_, data, ok, err := ReadClipboardImagePNG()
	if err != nil {
		t.Fatal(err)
	}
	if ok || data != nil {
		t.Fatalf("ok = %v, data = %v", ok, data)
	}
}

func TestReadClipboardImagePNGRejectsMalformedData(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("DISPLAY", "")
	writeFakeLinuxClipboard(t, "wl-paste", `
if [ "$1" = "--list-types" ]; then
    printf 'image/png\n'
else
    printf 'not a png'
fi
`)

	if _, _, ok, err := ReadClipboardImagePNG(); err == nil || ok {
		t.Fatalf("ok = %v, err = %v", ok, err)
	}
}

func TestReadClipboardImagePNGBackendFallback(t *testing.T) {
	for _, tt := range []struct {
		name    string
		xclip   string
		wantErr []string
		wantPNG bool
	}{
		{
			name:  "both fail",
			xclip: "printf 'X11 display unavailable' >&2\nexit 1\n",
			wantErr: []string{
				"wl-paste failed", "Wayland socket denied",
				"xclip failed", "X11 display unavailable",
			},
		},
		{
			name:    "X11 unavailable",
			wantErr: []string{"wl-paste failed", "Wayland socket denied"},
		},
		{
			name: "X11 succeeds",
			xclip: `if [ "$4" = "TARGETS" ]; then
    printf 'image/png\n'
else
    /bin/cat "$CLIPBOARD_IMAGE_FIXTURE"
fi
`,
			wantPNG: true,
		},
		{
			name:  "X11 contains no image",
			xclip: "printf 'text/plain\\n'\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			t.Setenv("WAYLAND_DISPLAY", "wayland-1")
			t.Setenv("DISPLAY", ":0")
			fixture := filepath.Join(dir, "image.png")
			if err := os.WriteFile(fixture, testClipboardPNG(t), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CLIPBOARD_IMAGE_FIXTURE", fixture)
			scripts := map[string]string{
				"wl-paste": "printf 'Wayland socket denied' >&2\nexit 1\n",
				"xclip":    tt.xclip,
			}
			for name, script := range scripts {
				if script == "" {
					continue
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
					t.Fatal(err)
				}
			}

			path, data, ok, err := ReadClipboardImagePNG()
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatal("expected clipboard backend errors")
				}
				for _, diagnostic := range tt.wantErr {
					if !strings.Contains(err.Error(), diagnostic) {
						t.Errorf("error %q does not contain %q", err, diagnostic)
					}
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if path != "" || ok != tt.wantPNG {
				t.Fatalf("path = %q, ok = %v, want PNG = %v", path, ok, tt.wantPNG)
			}
			if tt.wantPNG {
				if w, h := ImageDimensions(data); w != 2 || h != 2 {
					t.Fatalf("image dimensions = %dx%d", w, h)
				}
			} else if data != nil {
				t.Fatal("unexpected image data")
			}
		})
	}
}

func TestPreferredClipboardImageMIME(t *testing.T) {
	mime, ok := preferredClipboardImageMIME("text/plain\nimage/jpeg\nimage/png\n")
	if !ok || mime != "image/png" {
		t.Fatalf("mime = %q, ok = %v", mime, ok)
	}
	if mime, ok := preferredClipboardImageMIME("text/plain\ntext/html\n"); ok {
		t.Fatalf("mime = %q, want no image MIME", mime)
	}
}

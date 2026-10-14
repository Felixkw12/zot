package modes

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestClipboardImagePreviewsMatchActiveMarkersAndBudget(t *testing.T) {
	view := &tui.View{Theme: tui.Dark, ImageProto: tui.ImageProtocolNone}
	pending := []clipboardImageAttachment{
		testClipboardImage("[clipboard image #1]", "png-1"),
		testClipboardImage("[clipboard image #2]", "png-2"),
	}
	text := "[clipboard image #2]"
	lines := clipboardImagePreviews(view, text, pending, 80, 12)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, pending[0].Marker) || !strings.Contains(joined, pending[1].Marker) || !strings.Contains(joined, "image/png") {
		t.Fatalf("preview does not match active attachments: %q", joined)
	}
	if got := clipboardImagePreviews(view, "deleted", pending, 80, 12); len(got) != 0 {
		t.Fatal("deleted markers must hide previews")
	}
	for budget := 0; budget <= 12; budget++ {
		lines := clipboardImagePreviews(view, strings.Join([]string{pending[0].Marker, pending[1].Marker}, " "), pending, 80, budget)
		if len(lines) > budget {
			t.Fatalf("budget %d: got %d rows", budget, len(lines))
		}
	}
}

func TestClipboardImagePreviewsGraphicsFitBudget(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 20, 100))); err != nil {
		t.Fatal(err)
	}
	view := &tui.View{Theme: tui.Dark, ImageProto: tui.ImageProtocolKitty}
	pending := []clipboardImageAttachment{
		{Marker: "[clipboard image #1]", Image: provider.ImageBlock{MimeType: "image/png", Data: data.Bytes()}},
		{Marker: "[clipboard image #2]", Image: provider.ImageBlock{MimeType: "image/png", Data: data.Bytes()}},
	}
	text := pending[0].Marker + " " + pending[1].Marker
	for _, width := range []int{4, 20, 80} {
		for budget := 0; budget <= 12; budget++ {
			lines := clipboardImagePreviews(view, text, pending, width, budget)
			if len(lines) > budget {
				t.Fatalf("budget %d: got %d rows", budget, len(lines))
			}
			if width == 80 && budget == 12 && strings.Count(strings.Join(lines, ""), "\x1b_G") != 2 {
				t.Fatal("both active attachments should fit")
			}
		}
	}
}

func TestReadClipboardImageAndCleanupRemovesTemporaryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clipboard.png")
	if err := os.WriteFile(path, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}

	data, ok, err := readClipboardImageAndCleanup(func() (string, []byte, bool, error) {
		return path, []byte("png"), true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || string(data) != "png" {
		t.Fatalf("ok = %v, data = %q", ok, data)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temporary image still exists: %v", err)
	}
}

func testClipboardImage(marker string, data string) clipboardImageAttachment {
	return clipboardImageAttachment{
		Marker: marker,
		Image:  provider.ImageBlock{MimeType: "image/png", Data: []byte(data)},
	}
}

func TestPreparePromptWithClipboardImagesStripsPresentMarker(t *testing.T) {
	pending := []clipboardImageAttachment{testClipboardImage("[clipboard image #1]", "png-1")}

	text, images := preparePromptWithClipboardImages("describe this [clipboard image #1] please", pending)

	if text != "describe this please" {
		t.Fatalf("text = %q, want %q", text, "describe this please")
	}
	if len(images) != 1 {
		t.Fatalf("len(images) = %d, want 1", len(images))
	}
	if string(images[0].Data) != "png-1" {
		t.Fatalf("image data = %q, want png-1", string(images[0].Data))
	}
}

func TestPreparePromptWithClipboardImagesAllowsImageOnlyPrompt(t *testing.T) {
	pending := []clipboardImageAttachment{testClipboardImage("[clipboard image #1]", "png-1")}

	text, images := preparePromptWithClipboardImages("[clipboard image #1]", pending)

	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
	if len(images) != 1 {
		t.Fatalf("len(images) = %d, want 1", len(images))
	}
}

func TestPreparePromptWithClipboardImagesIgnoresDeletedMarker(t *testing.T) {
	pending := []clipboardImageAttachment{testClipboardImage("[clipboard image #1]", "png-1")}

	text, images := preparePromptWithClipboardImages("describe this from memory", pending)

	if text != "describe this from memory" {
		t.Fatalf("text = %q, want unchanged", text)
	}
	if len(images) != 0 {
		t.Fatalf("len(images) = %d, want 0", len(images))
	}
}

func TestPreparePromptWithClipboardImagesHandlesMultipleImagesInPasteOrder(t *testing.T) {
	pending := []clipboardImageAttachment{
		testClipboardImage("[clipboard image #1]", "png-1"),
		testClipboardImage("[clipboard image #2]", "png-2"),
	}

	text, images := preparePromptWithClipboardImages("compare [clipboard image #2] with [clipboard image #1]", pending)

	if text != "compare with" {
		t.Fatalf("text = %q, want %q", text, "compare with")
	}
	if len(images) != 2 {
		t.Fatalf("len(images) = %d, want 2", len(images))
	}
	if string(images[0].Data) != "png-1" || string(images[1].Data) != "png-2" {
		t.Fatalf("images not attached in paste order: %q, %q", string(images[0].Data), string(images[1].Data))
	}
}

func TestPreparePromptWithClipboardImagesDuplicateMarkerAttachesOnce(t *testing.T) {
	pending := []clipboardImageAttachment{testClipboardImage("[clipboard image #1]", "png-1")}

	text, images := preparePromptWithClipboardImages("[clipboard image #1] and again [clipboard image #1]", pending)

	if strings.Contains(text, "[clipboard image #1]") {
		t.Fatalf("text still contains marker: %q", text)
	}
	if text != "and again" {
		t.Fatalf("text = %q, want %q", text, "and again")
	}
	if len(images) != 1 {
		t.Fatalf("len(images) = %d, want 1", len(images))
	}
}

func TestPreparePromptWithClipboardImagesNoPendingPreservesWhitespace(t *testing.T) {
	input := "please review:\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"

	text, images := preparePromptWithClipboardImages(input, nil)

	if text != input {
		t.Fatalf("text changed without pending images:\n got %q\nwant %q", text, input)
	}
	if images != nil {
		t.Fatalf("images = %#v, want nil", images)
	}
}

func TestPreparePromptWithClipboardImagesPreservesMultilinePrompt(t *testing.T) {
	pending := []clipboardImageAttachment{testClipboardImage("[clipboard image #1]", "png-1")}
	input := "compare this image [clipboard image #1]\n\nwith this code:\n\treturn 1"
	want := "compare this image\n\nwith this code:\n\treturn 1"

	text, images := preparePromptWithClipboardImages(input, pending)

	if text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	if len(images) != 1 {
		t.Fatalf("len(images) = %d, want 1", len(images))
	}
}

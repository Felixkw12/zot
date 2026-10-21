package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Issue #227: some local endpoints reject valid PNG/WebP data URLs with
// this misleading base64 error. Exercise the actual HTTP and stream path.
func TestOpenAIImageRejectionFallback(t *testing.T) {
	pngBytes := encodePNG(t, makeRect(3, 2))
	webpBytes, err := base64.StdEncoding.DecodeString("UklGRh4AAABXRUJQVlA4TBEAAAAvAAAAAAfQ//73v/+BiOh/AAA=")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		mime string
		data []byte
	}{
		{"png", "image/png", pngBytes},
		{"webp", "image/webp", webpBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := image.DecodeConfig(bytes.NewReader(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			for _, toolResult := range []bool{false, true} {
				t.Run(fmt.Sprintf("tool=%t", toolResult), func(t *testing.T) {
					imageBlock := ImageBlock{MimeType: tc.mime, Data: tc.data}
					messages := []Message{{Role: RoleUser, Content: []Content{imageBlock}}}
					if toolResult {
						messages = []Message{
							{Role: RoleAssistant, Content: []Content{ToolCallBlock{ID: "read-1", Name: "read"}}},
							{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "read-1", Content: []Content{imageBlock}}}},
						}
					}
					before, err := json.Marshal(messages)
					if err != nil {
						t.Fatal(err)
					}
					requests := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests++
						var wire struct {
							Messages []struct {
								Role    string          `json:"role"`
								Content json.RawMessage `json:"content"`
							} `json:"messages"`
						}
						if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						var urls []string
						for _, msg := range wire.Messages {
							var parts []oaiContentImage
							if json.Unmarshal(msg.Content, &parts) != nil {
								continue
							}
							for _, part := range parts {
								if part.Type == "image_url" {
									if msg.Role != "user" {
										t.Errorf("image on role %q", msg.Role)
									}
									urls = append(urls, part.ImageURL.URL)
								}
							}
						}
						if len(urls) != 1 {
							t.Errorf("got %d images, want 1", len(urls))
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						if requests%2 == 1 {
							want := "data:" + tc.mime + ";base64," + base64.StdEncoding.EncodeToString(tc.data)
							if urls[0] != want {
								t.Error("first attempt changed the original image")
							}
							w.WriteHeader(http.StatusBadRequest)
							fmt.Fprint(w, `{"error":"'url' field must be a base64 encoded image."}`)
							return
						}
						const prefix = "data:image/jpeg;base64,"
						if !strings.HasPrefix(urls[0], prefix) {
							t.Error("retry is not a JPEG data URL")
						}
						data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(urls[0], prefix))
						if err != nil {
							t.Error(err)
						}
						decoded, format, err := image.Decode(bytes.NewReader(data))
						if err != nil {
							t.Error(err)
						} else if format != "jpeg" || decoded.Bounds().Dx() != cfg.Width || decoded.Bounds().Dy() != cfg.Height {
							t.Errorf("retry changed dimensions or did not really encode JPEG: %s %v", format, decoded.Bounds())
						}
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					}))
					defer server.Close()
					client := NewOpenAICompat("local-custom", "synthetic", server.URL+"/v1", "")
					// A second turn replays the same image, as does resuming a session.
					for range 2 {
						stream, err := client.Stream(context.Background(), Request{Model: "vision", Messages: messages})
						if err != nil {
							t.Fatal(err)
						}
						var text string
						for event := range stream {
							if done, ok := event.(EventDone); ok {
								if done.Err != nil {
									t.Fatal(done.Err)
								}
								for _, block := range done.Message.Content {
									if block, ok := block.(TextBlock); ok {
										text += block.Text
									}
								}
							}
						}
						if text != "ok" {
							t.Fatalf("response = %q", text)
						}
					}
					if requests != 4 {
						t.Fatalf("requests = %d, want 4", requests)
					}
					after, err := json.Marshal(messages)
					if err != nil || !bytes.Equal(before, after) {
						t.Fatal("fallback mutated the transcript")
					}
				})
			}
		})
	}
}

func TestOpenAIImageFallbackDoesNotRetryOtherFailures(t *testing.T) {
	pngBytes := encodePNG(t, makeRect(2, 2))
	for _, tc := range []struct {
		name    string
		status  int
		message string
		block   Content
	}{
		{"unrelated 400", 400, "invalid model", ImageBlock{MimeType: "image/png", Data: pngBytes}},
		{"wrong status", 422, "'url' field must be a base64 encoded image", ImageBlock{MimeType: "image/png", Data: pngBytes}},
		{"text only", 400, "'url' field must be a base64 encoded image", TextBlock{Text: "hello"}},
		{"invalid image", 400, "'url' field must be a base64 encoded image", ImageBlock{MimeType: "image/png", Data: []byte("invalid")}},
		{"JPEG already", 400, "'url' field must be a base64 encoded image", ImageBlock{MimeType: "image/jpeg", Data: encodeJPEG(t, makeRect(2, 2))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.message)
			}))
			defer server.Close()
			client := NewOpenAICompat("custom", "", server.URL, "")
			_, err := client.Stream(context.Background(), Request{Model: "vision", Messages: []Message{{Role: RoleUser, Content: []Content{tc.block}}}})
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v", err)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want 1", requests)
			}
		})
	}
}

func TestOpenAIImageFallbackRetryIsBounded(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"'url' field must be a base64 encoded image."}`)
	}))
	defer server.Close()
	client := NewOpenAICompat("custom", "", server.URL, "")
	_, err := client.Stream(context.Background(), Request{Model: "vision", Messages: []Message{{Role: RoleUser, Content: []Content{ImageBlock{MimeType: "image/png", Data: encodePNG(t, makeRect(2, 2))}}}}})
	if err == nil || requests != 2 {
		t.Fatalf("error = %v, requests = %d, want error after 2 requests", err, requests)
	}
}

func TestOpenAIImageFallbackSuccessfulRequestUnchanged(t *testing.T) {
	original := encodePNG(t, makeRect(2, 2))
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(original)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var wire struct {
			Messages []struct {
				Content []oaiContentImage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Error(err)
		} else if len(wire.Messages) != 1 || len(wire.Messages[0].Content) != 1 || wire.Messages[0].Content[0].ImageURL.URL != wantURL {
			t.Error("successful request changed original PNG")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	client := NewOpenAICompat("custom", "", server.URL, "")
	stream, err := client.Stream(context.Background(), Request{Model: "vision", Messages: []Message{{Role: RoleUser, Content: []Content{ImageBlock{MimeType: "image/png", Data: original}}}}})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestOpenAIImageFallbackPixelLimit(t *testing.T) {
	// A valid PNG header advertising too many pixels must be rejected before
	// allocating a decoded image, even though the entire input is tiny.
	data := encodePNG(t, makeRect(2, 2))
	binary.BigEndian.PutUint32(data[16:20], 5001)
	binary.BigEndian.PutUint32(data[20:24], 5000)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, ok := openAIImageJPEGFallback("data:image/png;base64," + base64.StdEncoding.EncodeToString(data)); ok {
		t.Fatal("converted an image exceeding the pixel limit")
	}
}

func TestOpenAIImageFallbackTransparency(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	converted, ok := openAIImageJPEGFallback(url)
	if !ok {
		t.Fatal("expected conversion")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(converted, "data:image/jpeg;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if got := color.NRGBAModel.Convert(img.At(0, 0)); !reflect.DeepEqual(got, color.NRGBA{R: 255, G: 255, B: 255, A: 255}) {
		t.Fatalf("transparent pixel = %v, want white", got)
	}
}

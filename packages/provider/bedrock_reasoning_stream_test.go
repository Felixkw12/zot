package provider

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Frames with no headers exercise the wrapped-payload event discriminator.
func bedrockReasoningFrame(payload string) []byte {
	frame := make([]byte, 16+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(frame)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	copy(frame[12:], payload)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}

func TestBedrockReasoningIsNotReplayedToOtherProtocols(t *testing.T) {
	blocks := []Content{
		ReasoningBlock{ID: bedrockReasoningID, Summary: "bedrock-private-summary", Encrypted: "bedrock-private-signature"},
		ReasoningBlock{ID: bedrockRedactedReasoningID, Encrypted: "bedrock-private-redacted"},
		TextBlock{Text: "answer"},
	}
	codex, err := (&codexClient{}).buildRequest(Request{Model: "gpt-5.5", Messages: []Message{{Role: RoleAssistant, Content: blocks}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []any{codex, convertGemAssistantParts(blocks, false)} {
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "bedrock-private") || !strings.Contains(string(data), "answer") {
			t.Fatalf("invalid provider switch request: %s", data)
		}
	}
}

func TestBedrockStreamsAndReplaysReasoning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	received := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.Write(bedrockReasoningFrame(`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"first "}}}}`))
		w.(http.Flusher).Flush()
		select {
		case <-received:
		case <-ctx.Done():
			return
		}
		for _, payload := range []string{
			`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"second"}}}}`,
			`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"reasoningContent":{"signature":"private-signature"}}}}`,
			`{"contentBlockStop":{"contentBlockIndex":0}}`,
			`{"contentBlockDelta":{"contentBlockIndex":1,"delta":{"reasoningContent":{"redactedContent":"cHJpdmF0ZQ=="}}}}`,
			`{"contentBlockStop":{"contentBlockIndex":1}}`,
			`{"contentBlockDelta":{"contentBlockIndex":2,"delta":{"text":"answer"}}}`,
			`{"contentBlockStop":{"contentBlockIndex":2}}`,
			`{"messageStop":{"stopReason":"end_turn"}}`,
		} {
			w.Write(bedrockReasoningFrame(payload))
		}
	}))
	defer server.Close()
	c := &bedrockClient{baseURL: server.URL, bearerToken: "test", region: "us-east-1", http: server.Client()}
	events, err := c.Stream(ctx, Request{Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	var reasoning, text string
	var done EventDone
	for event := range events {
		switch e := event.(type) {
		case EventReasoningDelta:
			if reasoning == "" {
				close(received)
			}
			reasoning += e.Delta
		case EventTextDelta:
			text += e.Delta
		case EventDone:
			done = e
		}
	}
	if done.Err != nil || reasoning != "first second" || text != "answer" || len(done.Message.Content) != 3 {
		t.Fatalf("reasoning=%q text=%q done=%+v", reasoning, text, done)
	}
	// Opaque data stays in replay metadata, never in live reasoning events.
	rb := done.Message.Content[0].(ReasoningBlock)
	if rb.Encrypted != "private-signature" || rb.ID != bedrockReasoningID {
		t.Fatalf("reasoning metadata = %+v", rb)
	}
	req, err := c.buildRequest(Request{Model: "test-model", Messages: []Message{done.Message}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(req.Messages[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []struct {
		ReasoningContent struct {
			ReasoningText struct {
				Text      string `json:"text"`
				Signature string `json:"signature"`
			} `json:"reasoningText"`
			RedactedContent string `json:"redactedContent"`
		} `json:"reasoningContent"`
	}
	if err := json.Unmarshal(data, &blocks); err != nil {
		t.Fatal(err)
	}
	if blocks[0].ReasoningContent.ReasoningText.Text != "first second" || blocks[0].ReasoningContent.ReasoningText.Signature != "private-signature" || blocks[1].ReasoningContent.RedactedContent != "cHJpdmF0ZQ==" {
		t.Fatalf("replay = %s", data)
	}
	foreign, err := c.buildRequest(Request{Model: "test-model", Messages: []Message{{Role: RoleAssistant, Content: []Content{ReasoningBlock{Summary: "foreign", Encrypted: "foreign"}, TextBlock{Text: "answer"}}}}})
	if err != nil || len(foreign.Messages[0].Content) != 1 {
		t.Fatalf("foreign reasoning leaked: %+v, %v", foreign, err)
	}
}

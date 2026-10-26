package provider

import "strings"

// ReadableReasoningMetaKey stores a display-only reasoning tail on an assistant
// message. It must not be included in model inputs or replace replay metadata.
const ReadableReasoningMetaKey = "readable_reasoning"

// ReadableReasoning returns display text only, never signatures or encrypted
// payloads. Older sessions may carry readable summaries in replay blocks.
func (m Message) ReadableReasoning() string {
	if text := m.Meta[ReadableReasoningMetaKey]; text != "" {
		return text
	}
	var summaries []string
	for _, content := range m.Content {
		if block, ok := content.(ReasoningBlock); ok && block.Summary != "" {
			summaries = append(summaries, block.Summary)
		}
	}
	return strings.Join(summaries, "\n\n")
}

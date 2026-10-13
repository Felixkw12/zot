package continuous

import (
	"encoding/json"
	"strings"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func checkNestedIntegrity(snap storage.Snapshot, bad func(string) error) error {
	seen := map[string]bool{}
	calls := map[string]bool{}
	err := pageAll(snap, "task/", func(row storage.Record) error {
		var task Task
		if json.Unmarshal(row.Value, &task) != nil || task.Kind != TaskKindTool {
			return nil
		}
		in, _, err := decodeTool(task)
		if err != nil {
			return bad("invalid tool task input")
		}
		if in.ParentTask == "" {
			if in.Depth != 0 || in.NestedKey != "" {
				return bad("nested tool without parent")
			}
			return nil
		}
		parent, ok, err := read[Task](snap, taskKey(in.ParentTask))
		if err != nil || !ok || parent.Kind != TaskKindTool || parent.ConversationID != task.ConversationID || task.Owner != parent.ID || task.Background {
			return bad("nested tool ownership mismatch")
		}
		parentInput, _, err := decodeTool(parent)
		if err != nil || parentInput.RunID != in.RunID || in.Depth != parentInput.Depth+1 || in.Depth < 1 || in.Depth > 3 {
			return bad("nested tool run or depth mismatch")
		}
		if in.NestedKey == "" || len(in.NestedKey) > 128 || strings.ContainsAny(in.NestedKey, "/\x00") || in.CallID != parentInput.CallID+"/n/"+in.NestedKey {
			return bad("invalid nested tool key")
		}
		key := in.ParentTask + "/" + in.NestedKey
		if seen[key] {
			return bad("duplicate nested tool key")
		}
		seen[key] = true
		calls[task.ConversationID+"\x00"+in.CallID] = true
		if task.State == "terminal" {
			var outcome toolOutcome
			if json.Unmarshal(task.Result, &outcome) != nil || outcome.Entry != 0 || outcome.Nested == nil {
				return bad("nested tool result or transcript mismatch")
			}
			if _, err := outcome.Nested.result(); err != nil {
				return bad("invalid nested tool result")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return pageAll(snap, "entry/", func(row storage.Record) error {
		var entry Entry
		if json.Unmarshal(row.Value, &entry) != nil {
			return bad("invalid transcript entry")
		}
		if len(entry.Message) == 0 {
			return nil
		}
		message, err := core.DecodeMessage(entry.Message)
		if err != nil {
			return bad("invalid transcript message")
		}
		for _, block := range message.Content {
			id := ""
			switch block := block.(type) {
			case provider.ToolCallBlock:
				id = block.ID
			case provider.ToolResultBlock:
				id = block.CallID
			}
			if calls[entry.ConversationID+"\x00"+id] {
				return bad("nested tool wrote transcript entry")
			}
		}
		return nil
	})
}

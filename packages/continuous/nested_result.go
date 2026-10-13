package continuous

import (
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

const maxNestedResultBytes = 256 * 1024

// storedNestedResult is a nested call's result as stored and returned. It
// carries data only. Control signals (handoff, terminate, tool activation)
// apply only to model-issued calls, so they are never stored for a nested
// call and cannot reach the caller or the run.
type storedNestedResult struct {
	Message    json.RawMessage            `json:"message"`
	Status     string                     `json:"status,omitempty"`
	Structured json.RawMessage            `json:"structured,omitempty"`
	State      map[string]json.RawMessage `json:"state,omitempty"`
	Usage      *provider.Usage            `json:"usage,omitempty"`
	Nested     []provider.NestedToolCall  `json:"nested_calls,omitempty"`
	Truncated  bool                       `json:"truncated,omitempty"`
}

func storeNestedResult(result core.ToolResult) storedNestedResult {
	return storedNestedResult{Message: marshalMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "nested", Content: result.Content, IsError: result.IsError}}}), Status: result.Status, Structured: result.StructuredContent, State: result.State, Usage: result.Usage, Nested: result.NestedCalls}
}

func (s storedNestedResult) result() (core.ToolResult, error) {
	message, err := core.DecodeMessage(s.Message)
	if err != nil || len(message.Content) != 1 {
		return core.ToolResult{}, fmt.Errorf("invalid stored nested result")
	}
	block, ok := message.Content[0].(provider.ToolResultBlock)
	if !ok {
		return core.ToolResult{}, fmt.Errorf("invalid stored nested result block")
	}
	return core.ToolResult{Content: block.Content, IsError: block.IsError, Status: s.Status, StructuredContent: s.Structured, State: s.State, Usage: s.Usage, NestedCalls: s.Nested}, nil
}

func boundedNestedResult(result core.ToolResult) storedNestedResult {
	// Remove large blocks rather than storing an unbounded outcome. The exact
	// committed value is also the value returned to every caller and replay.
	result.Content = append([]provider.Content(nil), result.Content...)
	truncated := false
	for {
		stored := storeNestedResult(result)
		stored.Truncated = truncated
		data, err := json.Marshal(stored)
		if err == nil && len(data) <= maxNestedResultBytes {
			return stored
		}
		reduced := false
		for i := len(result.Content) - 1; i >= 0; i-- {
			switch block := result.Content[i].(type) {
			case provider.ImageBlock:
				result.Content = append(result.Content[:i], result.Content[i+1:]...)
				reduced = true
			case provider.TextBlock:
				if len(block.Text) > 256 {
					end := len(block.Text) / 2
					for end > 0 && !utf8.RuneStart(block.Text[end]) {
						end--
					}
					block.Text = block.Text[:end] + "\n[nested tool result truncated]"
					result.Content[i] = block
					reduced = true
				}
			}
			if reduced {
				break
			}
		}
		if !reduced {
			result = core.ToolResult{IsError: true, Status: "failed", Content: []provider.Content{provider.TextBlock{Text: "nested tool result exceeded the 256 KiB limit"}}}
		}
		truncated = true
	}
}

func commitNestedResult(tx *TaskTx, in toolInput, result core.ToolResult, status, notice, outcome string) (Next, error) {
	nested, err := committedNestedCalls(tx.View(), tx.Task().ID)
	if err != nil {
		return Next{}, err
	}
	result.NestedCalls, result.Status = nested, status
	stored := boundedNestedResult(result)
	if _, ok := tx.Snapshot.Get(progressKey(tx.Task().ID)); ok {
		tx.Delete(progressKey(tx.Task().ID))
	}
	if notice != "" {
		if chain, ok, err := read[Chain](tx.View(), chainKey(tx.Task().ConversationID)); err != nil {
			return Next{}, err
		} else if ok {
			chain.Notices = append(chain.Notices, notice)
			chain.Revision = tx.Snapshot.Revision() + 1
			tx.Put(chainKey(chain.ConversationID), chain)
		}
	}
	if result.Usage != nil {
		c, err := conversation(tx.View(), tx.Task().ConversationID)
		if err != nil {
			return Next{}, err
		}
		c.UsageSequence++
		c.Revision = tx.Snapshot.Revision() + 1
		tx.Put(usageKey(c.ID, c.UsageSequence), UsageRecord{ConversationID: c.ID, RunID: in.RunID, Turn: in.Turn, Provider: c.Config.Provider, Model: c.Config.Model, Status: "known", Source: "tool", Tool: in.Name, Usage: *result.Usage, Time: time.Now().UTC(), TaskID: tx.Task().ID})
		tx.Put("conversation/"+c.ID, c)
	}
	return Next{Outcome: outcome, Result: toolOutcome{Status: status, Nested: &stored}, Error: notice}, nil
}

func committedNestedCalls(snap storage.Snapshot, parent string) ([]provider.NestedToolCall, error) {
	var children []Task
	err := pageAll(snap, "task-owner/"+parent+"/", func(row storage.Record) error {
		var id string
		if err := json.Unmarshal(row.Value, &id); err != nil {
			return err
		}
		child, ok, err := read[Task](snap, taskKey(id))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("nested child missing")
		}
		if child.Kind == TaskKindTool {
			children = append(children, child)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortTasks(children)
	var calls []provider.NestedToolCall
	for _, child := range children {
		in, _, err := decodeTool(child)
		if err != nil {
			return nil, err
		}
		if in.ParentTask != parent || child.State != "terminal" {
			continue
		}
		var outcome toolOutcome
		if json.Unmarshal(child.Result, &outcome) != nil || outcome.Nested == nil {
			return nil, fmt.Errorf("nested child has no committed result")
		}
		result, err := outcome.Nested.result()
		if err != nil {
			return nil, err
		}
		calls = append(calls, provider.NestedToolCall{ID: in.CallID, Name: in.Name, Args: in.Args, Result: core.ToolResultText(result), IsError: result.IsError, Status: outcome.Status, Executed: outcome.Executed})
		calls = append(calls, result.NestedCalls...)
	}
	return calls, nil
}

func nestedMetadata(id string, calls []provider.NestedToolCall) map[string]string {
	if len(calls) == 0 {
		return nil
	}
	data, _ := json.Marshal(map[string][]provider.NestedToolCall{id: calls})
	return map[string]string{provider.NestedToolCallsMetaKey: string(data)}
}

package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestCodemodeShortArgs(t *testing.T) {
	t.Setenv("ZOT_TOOL_ARG_WIDTH", "")
	code := "const café = 1;\nreturn café;"
	raw, _ := json.Marshal(map[string]string{"code": code})
	if got := ShortArgs("codemode", raw); got != "const café = 1; return café;" {
		t.Fatalf("summary = %q", got)
	}
	long, _ := json.Marshal(map[string]string{"code": strings.Repeat("界", 100)})
	if got := ShortArgs("codemode", long); !utf8.ValidString(got) || visibleWidth(got) > toolArgWidth() || !strings.HasSuffix(got, "...") {
		t.Fatalf("invalid truncated summary: %q", got)
	}
	if got := ShortArgs("other", raw); !strings.Contains(got, `"code"`) {
		t.Fatalf("unrelated tool summary changed: %q", got)
	}
}

func TestCodemodeRendering(t *testing.T) {
	code := "const uniqueSource = 42;\nreturn uniqueSource;"
	object, _ := json.Marshal(map[string]string{"code": code})
	quoted, _ := json.Marshal(code)
	for _, args := range []string{string(object), string(quoted), code} {
		for _, layout := range []string{"box", "flat", "compact"} {
			for _, width := range []int{24, 80} {
				for _, result := range []string{"", "RESULT_MARKER", "execution failed"} {
					v := View{Theme: Dark, ExpandAll: true, FlatTools: layout == "flat", CompactMode: layout == "compact"}
					tc := ToolCallView{ID: "script", Name: "codemode", RawJSONBuf: args, Args: ShortArgs("codemode", json.RawMessage(args)), Done: true, Result: result, Error: result == "execution failed"}
					// The transcript path must retain the script after the live overlay disappears.
					v.Messages = []provider.Message{
						{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: tc.ID, Name: tc.Name, Arguments: json.RawMessage(args)}}},
						{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: tc.ID, IsError: tc.Error, Content: []provider.Content{provider.TextBlock{Text: result}}}}},
					}
					v.refreshToolPaths()
					for _, rows := range [][]string{v.RenderToolCall(tc, width), v.renderMessage(v.Messages[1], width, false)} {
						plain := stripANSI(strings.Join(rows, "\n"))
						if !strings.Contains(plain, "Script") || (width == 80 && !strings.Contains(plain, "Script (JavaScript)")) {
							t.Fatalf("missing script section (%s, %d): %s", layout, width, plain)
						}
						if width == 80 && !strings.Contains(plain, "return uniqueSource;") {
							t.Fatalf("missing source: %s", plain)
						}
						if result != "" && width == 80 && (strings.Index(plain, result) < 0 || strings.Index(plain, result) > strings.Index(plain, "Script (JavaScript)")) {
							t.Fatalf("result must precede source: %s", plain)
						}
						for _, row := range rows {
							if visibleWidth(row) > width {
								t.Fatalf("row exceeds %d cells: %q", width, stripANSI(row))
							}
						}
					}
				}
			}
		}
	}
}

func TestCodemodeStreamingPreview(t *testing.T) {
	v := View{Theme: Dark, ExpandAll: true}
	tc := ToolCallView{Name: "codemode", Streaming: true, RawJSONBuf: `{"code":"const streamingSource = 42;\nreturn streamingSource`}
	plain := stripANSI(strings.Join(v.RenderToolCall(tc, 80), "\n"))
	if !strings.Contains(plain, "const streamingSource = 42;") || !strings.Contains(plain, "return streamingSource") {
		t.Fatalf("missing partial source: %s", plain)
	}
	body := strings.Join(v.renderLiveToolContent(tc, 80), "\n")
	if body == stripANSI(body) {
		t.Fatal("script preview is not syntax highlighted")
	}
	tc.Name = "other"
	if body := v.renderLiveToolContent(tc, 80); len(body) != 0 {
		t.Fatalf("unrelated code argument rendered: %v", body)
	}
}

func TestCodemodeReplayCache(t *testing.T) {
	v := View{Theme: Dark, ExpandAll: true, Messages: []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "script", Name: "codemode", Arguments: json.RawMessage(`{"code":"return 'OLD_SOURCE';"}`)}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "script", Content: []provider.Content{provider.TextBlock{Text: "RESULT_MARKER"}}}}},
	}}
	before := stripANSI(strings.Join(v.Build(80), "\n"))
	if !strings.Contains(before, "Script (JavaScript)") || !strings.Contains(before, "OLD_SOURCE") {
		t.Fatalf("missing replay source: %s", before)
	}
	v.Messages[0].Content[0] = provider.ToolCallBlock{ID: "script", Name: "codemode", Arguments: json.RawMessage(`{"code":"return 'NEW_SOURCE';"}`)}
	after := stripANSI(strings.Join(v.Build(80), "\n"))
	if strings.Contains(after, "OLD_SOURCE") || strings.Count(after, "NEW_SOURCE") != 2 {
		t.Fatalf("stale result source in render cache: %s", after)
	}
}

func TestCodemodeTypedResult(t *testing.T) {
	v := View{Theme: Dark, ExpandAll: true}
	tc := ToolCallView{Name: "codemode", RawJSONBuf: `{"code":"return 'source';"}`, Done: true, ResultContent: []provider.Content{
		provider.TextBlock{Text: "**formatted result**", Format: "markdown"},
	}}
	plain := stripANSI(strings.Join(v.RenderToolCall(tc, 80), "\n"))
	if strings.Contains(plain, "**formatted result**") || !strings.Contains(plain, "formatted result") || !strings.Contains(plain, "return 'source';") {
		t.Fatalf("typed result or source lost: %s", plain)
	}
}

func TestCodemodeCollapse(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"code": strings.Repeat("text('source');\n", ToolCollapseLines+10) + "return 'SOURCE_END';"})
	for _, collapsed := range []bool{false, true} {
		v := View{Theme: Dark, CollapseToolCall: collapsed}
		tc := ToolCallView{ID: "script", Name: "codemode", RawJSONBuf: string(args), Result: "RESULT_MARKER", Done: true}
		v.Messages = []provider.Message{
			{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: tc.ID, Name: tc.Name, Arguments: args}}},
			{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: tc.ID, Content: []provider.Content{provider.TextBlock{Text: tc.Result}}}}},
		}
		v.refreshToolPaths()
		for _, expanded := range []bool{false, true} {
			v.ExpandAll = expanded
			for _, rows := range [][]string{v.RenderToolCall(tc, 80), v.renderMessage(v.Messages[1], 80, false)} {
				plain := stripANSI(strings.Join(rows, "\n"))
				if !strings.Contains(plain, "RESULT_MARKER") {
					t.Fatalf("result hidden: %s", plain)
				}
				if strings.Contains(plain, "SOURCE_END") != expanded {
					t.Fatalf("unexpected source expansion: %s", plain)
				}
				if !expanded && !collapsed && !strings.Contains(plain, "ctrl+o to expand") {
					t.Fatalf("missing expansion hint: %s", plain)
				}
			}
		}
	}
}

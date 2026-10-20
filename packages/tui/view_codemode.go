package tui

import (
	"encoding/json"
	"strings"
)

// codemodeSource also accepts the legacy string and raw-source forms. Normal
// provider events and stored calls use an object with a code field.
func codemodeSource(name, raw string) string {
	if name != "codemode" {
		return ""
	}
	input := strings.TrimSpace(raw)
	if strings.HasPrefix(input, "{") {
		var args struct {
			Code string `json:"code"`
		}
		if json.Unmarshal([]byte(input), &args) == nil {
			return args.Code
		}
		code, _, _ := ExtractPartialStringField(input, "code")
		return code
	}
	if strings.HasPrefix(input, `"`) {
		var code string
		if json.Unmarshal([]byte(input), &code) == nil {
			return code
		}
		return ""
	}
	return raw
}

func codemodeSummary(code string) string {
	summary := oneLineToolLabel(code)
	if visibleWidth(summary) > toolArgWidth() {
		summary = truncateToWidth(summary, toolArgWidth()-3) + "..."
	}
	return summary
}

func (v *View) renderCodemodeSource(code string, width int) []string {
	if strings.TrimSpace(code) == "" {
		return nil
	}
	bodyWidth := toolBoxBodyRenderWidth(width)
	if v.FlatTools || v.CompactMode {
		bodyWidth = flatToolBodyRenderWidth(width)
	}
	inner := max(1, bodyWidth-4)
	var rows []string
	for _, line := range v.Theme.HighlightCode(expandTabs(code), "javascript") {
		for _, wrapped := range wrapANSILine(line, inner) {
			rows = append(rows, "    "+wrapped)
		}
	}
	return rows
}

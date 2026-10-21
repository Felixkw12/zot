package modes

import (
	"fmt"
	"strings"
	"sync"

	"github.com/mattn/go-runewidth"

	"github.com/patriceckhart/zot/packages/mcp"
	"github.com/patriceckhart/zot/packages/tui"
)

type mcpAddStep int

const (
	mcpAddClosed mcpAddStep = iota
	mcpAddScope
	mcpAddTransport
	mcpAddName
	mcpAddTarget
	mcpAddToken
	mcpAddExposure
	mcpAddReview
)

// mcpAddDialog collects one new MCP server entry step by step. It only
// builds the entry, saving and reloading are left to the caller.
type mcpAddDialog struct {
	mu sync.Mutex

	step    mcpAddStep
	cursor  int
	input   string
	message string

	userPath    string
	projectPath string
	project     bool
	remote      bool
	name        string
	target      string // command line or URL
	tokenEnv    string
	exposure    string
	existing    map[string]bool // normalized names already configured
	width       int             // last render width, for the caret row
}

// mcpAddResult is a completed entry ready to be written to Path.
type mcpAddResult struct {
	Path   string
	Name   string
	Config mcp.ServerConfig
}

var mcpExposureChoices = []struct{ value, label, hint string }{
	{"deferred", "deferred", "found through mcp__search_tools (recommended)"},
	{"direct", "direct", "always advertised to the model"},
	{"hidden", "hidden", "connected, no tools registered"},
}

func newMCPAddDialog() *mcpAddDialog { return &mcpAddDialog{} }

// Open starts the wizard. userPath and projectPath are the files the MCP
// manager reads, so a saved server is picked up by the next reload.
func (d *mcpAddDialog) Open(userPath, projectPath string, existing ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.existing = map[string]bool{}
	for _, name := range existing {
		d.existing[normalizeServerName(name)] = true
	}
	d.step, d.cursor, d.input, d.message = mcpAddScope, 0, "", ""
	d.userPath, d.projectPath = userPath, projectPath
	d.project, d.remote = false, false
	d.name, d.target, d.tokenEnv, d.exposure = "", "", "", "deferred"
}

func (d *mcpAddDialog) Active() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.step != mcpAddClosed
}

func (d *mcpAddDialog) Close() {
	d.mu.Lock()
	d.step = mcpAddClosed
	d.mu.Unlock()
}

func (d *mcpAddDialog) path() string {
	if d.project {
		return d.projectPath
	}
	return d.userPath
}

func (d *mcpAddDialog) result() (mcpAddResult, error) {
	cfg := mcp.ServerConfig{}
	if d.exposure != "deferred" {
		cfg.Exposure = d.exposure
	}
	if d.remote {
		cfg.URL = d.target
		if d.tokenEnv != "" {
			cfg.Headers = map[string]string{"Authorization": "Bearer ${" + d.tokenEnv + "}"}
		}
	} else {
		words, err := mcp.SplitCommandLine(d.target)
		if err != nil {
			return mcpAddResult{}, err
		}
		cfg.Command, cfg.Args = words[0], words[1:]
	}
	return mcpAddResult{Path: d.path(), Name: d.name, Config: cfg}, nil
}

// back returns to the previous step, skipping the token step for stdio.
func (d *mcpAddDialog) back() {
	d.message = ""
	switch d.step {
	case mcpAddScope:
		d.step = mcpAddClosed
	case mcpAddTransport:
		d.step, d.cursor = mcpAddScope, boolIndex(d.project)
	case mcpAddName:
		d.step, d.cursor = mcpAddTransport, boolIndex(d.remote)
	case mcpAddTarget:
		d.step, d.input = mcpAddName, d.name
	case mcpAddToken:
		d.step, d.input = mcpAddTarget, d.target
	case mcpAddExposure:
		if d.remote {
			d.step, d.input = mcpAddToken, d.tokenEnv
		} else {
			d.step, d.input = mcpAddTarget, d.target
		}
	case mcpAddReview:
		d.step, d.cursor = mcpAddExposure, exposureIndex(d.exposure)
	}
}

func boolIndex(b bool) int {
	if b {
		return 1
	}
	return 0
}

func exposureIndex(value string) int {
	for i, c := range mcpExposureChoices {
		if c.value == value {
			return i
		}
	}
	return 0
}

func (d *mcpAddDialog) choices() int {
	switch d.step {
	case mcpAddScope, mcpAddTransport:
		return 2
	case mcpAddExposure:
		return len(mcpExposureChoices)
	case mcpAddReview:
		return 2
	}
	return 0
}

func (d *mcpAddDialog) textStep() bool {
	return d.step == mcpAddName || d.step == mcpAddTarget || d.step == mcpAddToken
}

// HandleKey advances the wizard. It returns a result once the user confirms
// the review step.
func (d *mcpAddDialog) HandleKey(k tui.Key) (mcpAddResult, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.step == mcpAddClosed {
		return mcpAddResult{}, false
	}
	switch k.Kind {
	case tui.KeyCtrlC:
		d.step = mcpAddClosed
		return mcpAddResult{}, false
	case tui.KeyEsc:
		d.back()
		return mcpAddResult{}, false
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
		return mcpAddResult{}, false
	case tui.KeyDown:
		if d.cursor+1 < d.choices() {
			d.cursor++
		}
		return mcpAddResult{}, false
	}
	if d.textStep() {
		switch k.Kind {
		case tui.KeyBackspace:
			if runes := []rune(d.input); len(runes) > 0 {
				d.input = string(runes[:len(runes)-1])
			}
		case tui.KeyPaste:
			d.input += strings.Join(strings.Fields(k.Paste), " ")
		case tui.KeyRune:
			if !k.Ctrl && !k.Alt && k.Rune >= 0x20 {
				d.input += string(k.Rune)
			}
		}
	}
	if k.Kind != tui.KeyEnter {
		return mcpAddResult{}, false
	}
	d.message = ""
	value := strings.TrimSpace(d.input)
	switch d.step {
	case mcpAddScope:
		d.project = d.cursor == 1
		d.step, d.cursor = mcpAddTransport, boolIndex(d.remote)
	case mcpAddTransport:
		d.remote = d.cursor == 1
		d.step, d.input = mcpAddName, d.name
	case mcpAddName:
		if !mcp.ValidServerName(value) {
			d.message = "use letters, digits, underscores, or hyphens"
			break
		}
		if d.existing[normalizeServerName(value)] {
			d.message = "a server named " + value + " already exists"
			break
		}
		d.name = value
		d.step, d.input = mcpAddTarget, d.target
	case mcpAddTarget:
		probe := mcp.ServerConfig{Command: value}
		if d.remote {
			probe = mcp.ServerConfig{URL: value}
		} else if words, err := mcp.SplitCommandLine(value); err != nil || len(words) == 0 {
			d.message = "enter a command"
			if err != nil {
				d.message = err.Error()
			}
			break
		}
		if err := mcp.CheckServer(probe); err != nil {
			d.message = err.Error()
			break
		}
		d.target = value
		if d.remote {
			d.step, d.input = mcpAddToken, d.tokenEnv
		} else {
			d.step, d.cursor = mcpAddExposure, exposureIndex(d.exposure)
		}
	case mcpAddToken:
		if value != "" && !envName(value) {
			d.message = "enter an environment variable name, not the token itself"
			break
		}
		d.tokenEnv = value
		d.step, d.cursor = mcpAddExposure, exposureIndex(d.exposure)
	case mcpAddExposure:
		d.exposure = mcpExposureChoices[d.cursor].value
		d.step, d.cursor = mcpAddReview, 0
	case mcpAddReview:
		if d.cursor == 1 {
			d.back()
			return mcpAddResult{}, false
		}
		result, err := d.result()
		if err != nil {
			d.message = err.Error()
			return mcpAddResult{}, false
		}
		d.step = mcpAddClosed
		return result, true
	}
	return mcpAddResult{}, false
}

func normalizeServerName(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "-", "_")
}

func envName(s string) bool {
	for i, r := range s {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return s != ""
}

func (d *mcpAddDialog) stepNumber() (int, int) {
	order := []mcpAddStep{mcpAddScope, mcpAddTransport, mcpAddName, mcpAddTarget}
	if d.remote {
		order = append(order, mcpAddToken)
	}
	order = append(order, mcpAddExposure, mcpAddReview)
	for i, s := range order {
		if s == d.step {
			return i + 1, len(order)
		}
	}
	return 0, len(order)
}

func (d *mcpAddDialog) prompt() (title, hint string) {
	switch d.step {
	case mcpAddScope:
		return "Where should the server be saved?", ""
	case mcpAddTransport:
		return "How does zot reach the server?", ""
	case mcpAddName:
		return "Server name", "used in tool names as mcp__<name>__<tool>"
	case mcpAddTarget:
		if d.remote {
			return "Server URL", "streamable HTTP endpoint, for example https://example.com/mcp"
		}
		return "Command", "executable and arguments, quotes group words, no shell expansion"
	case mcpAddToken:
		return "Bearer token variable (optional)", "name of an environment variable holding the token, leave empty for none"
	case mcpAddExposure:
		return "Tool exposure", ""
	case mcpAddReview:
		return "Review", ""
	}
	return "", ""
}

func (d *mcpAddDialog) Render(th tui.Theme, width int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.step == mcpAddClosed {
		return nil
	}
	d.width = width
	n, total := d.stepNumber()
	title, hint := d.prompt()
	lines := []string{
		frameHeader(th, fmt.Sprintf("Add MCP server  %d/%d", n, total), width),
		"  " + th.FG256(th.Accent, title),
	}
	if hint != "" {
		for _, row := range tui.WrapANSILine(hint, max(1, width-2)) {
			lines = append(lines, th.FG256(th.Muted, "  "+row))
		}
	}
	lines = append(lines, "")
	option := func(index int, label, detail string) {
		line := "  " + runewidth.FillRight(label, 15)
		if index == d.cursor {
			// Inner colors would reset the highlight background.
			lines = append(lines, th.PadHighlight(runewidth.Truncate(line+detail, width, "…"), width))
		} else {
			lines = append(lines, th.FG256(th.FG, line)+th.FG256(th.Muted, runewidth.Truncate(detail, max(0, width-17), "…")))
		}
	}
	switch d.step {
	case mcpAddScope:
		option(0, "user", friendlyPath(d.userPath))
		option(1, "project", friendlyPath(d.projectPath))
	case mcpAddTransport:
		option(0, "local command", "stdio, zot starts the process")
		option(1, "remote URL", "streamable HTTP")
	case mcpAddName, mcpAddTarget, mcpAddToken:
		lines = append(lines, th.FG256(th.FG, "  "+visibleInput(d.input, width)))
	case mcpAddExposure:
		for i, c := range mcpExposureChoices {
			option(i, c.label, c.hint)
		}
	case mcpAddReview:
		row := func(key, value string) {
			prefix := th.FG256(th.Muted, fmt.Sprintf("  %-10s", key))
			for idx, part := range tui.WrapANSILine(value, max(1, width-12)) {
				if idx > 0 {
					prefix = strings.Repeat(" ", 12)
				}
				lines = append(lines, prefix+part)
			}
		}
		row("file", friendlyPath(d.path()))
		row("name", d.name)
		if d.remote {
			row("url", d.target)
			if d.tokenEnv != "" {
				row("auth", "Bearer ${"+d.tokenEnv+"}")
			}
		} else {
			row("command", d.target)
		}
		row("exposure", d.exposure)
		lines = append(lines, "")
		option(0, "Save and reload MCP", "")
		option(1, "Back", "")
	}
	if d.message != "" {
		lines = append(lines, "", th.FG256(th.Error, "  "+d.message))
	}
	keys := []string{"enter continue", "esc back", "ctrl+c close"}
	if d.step == mcpAddReview {
		keys[0] = "enter confirm"
	}
	if !d.textStep() {
		keys = append([]string{"↑↓ choose"}, keys...)
	}
	lines = append(lines, "", renderKeyHints(th, keys, width), frameRule(th, width))
	return lines
}

// CursorPos returns the caret location for text steps inside the padded
// dialog, or -1 when no text is being edited.
func (d *mcpAddDialog) CursorPos() (row, col int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.textStep() {
		return -1, -1
	}
	// header, frame padding, title, hint rows, blank, input
	_, hint := d.prompt()
	hintRows := 0
	if hint != "" {
		hintRows = len(tui.WrapANSILine(hint, max(1, d.width-2)))
	}
	return 4 + hintRows, 2 + runewidth.StringWidth(visibleInput(d.input, d.width))
}

// visibleInput keeps the end of long input visible, leaving room for the
// two-column indent and the caret.
func visibleInput(input string, width int) string {
	avail := width - 3
	if width <= 0 || runewidth.StringWidth(input) <= avail {
		return input
	}
	runes := []rune(input)
	for len(runes) > 0 && runewidth.StringWidth(string(runes))+1 > avail {
		runes = runes[1:]
	}
	return "…" + string(runes)
}

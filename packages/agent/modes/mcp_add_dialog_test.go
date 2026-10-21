package modes

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/mcp"
	"github.com/patriceckhart/zot/packages/tui"
)

func typeInto(d *mcpAddDialog, text string) {
	for _, r := range text {
		d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: r})
	}
}

func enter(d *mcpAddDialog) (mcpAddResult, bool) {
	return d.HandleKey(tui.Key{Kind: tui.KeyEnter})
}

func TestMCPAddDialogBuildsRemoteServer(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	d := newMCPAddDialog()
	d.Open(filepath.Join(home, "mcp.json"), filepath.Join(cwd, ".zot", "mcp.json"))
	d.HandleKey(tui.Key{Kind: tui.KeyDown}) // project
	enter(d)
	d.HandleKey(tui.Key{Kind: tui.KeyDown}) // remote
	enter(d)
	typeInto(d, "bad name")
	enter(d)
	if d.step != mcpAddName || d.message == "" {
		t.Fatal("invalid name accepted")
	}
	for range "bad name" {
		d.HandleKey(tui.Key{Kind: tui.KeyBackspace})
	}
	typeInto(d, "docs")
	enter(d)
	typeInto(d, "https://example.test/mcp")
	enter(d)
	typeInto(d, "sk-literal-token")
	enter(d)
	if d.step != mcpAddToken {
		t.Fatal("literal token accepted as variable name")
	}
	d.input = ""
	typeInto(d, "DOCS_TOKEN")
	enter(d)
	d.HandleKey(tui.Key{Kind: tui.KeyDown}) // direct
	enter(d)
	rendered := stripANSIBytes(strings.Join(d.Render(tui.Theme{}, 80), "\n"))
	if !strings.Contains(rendered, "Bearer ${DOCS_TOKEN}") || !strings.Contains(rendered, "7/7") {
		t.Fatalf("review=%s", rendered)
	}
	result, done := enter(d)
	want := mcpAddResult{Path: filepath.Join(cwd, ".zot", "mcp.json"), Name: "docs", Config: mcp.ServerConfig{
		URL: "https://example.test/mcp", Headers: map[string]string{"Authorization": "Bearer ${DOCS_TOKEN}"}, Exposure: "direct",
	}}
	if !done || !reflect.DeepEqual(result, want) || d.Active() {
		t.Fatalf("result=%+v done=%v", result, done)
	}
}

func TestMCPAddDialogBuildsCommandAndNavigatesBack(t *testing.T) {
	home := t.TempDir()
	d := newMCPAddDialog()
	d.Open(filepath.Join(home, "mcp.json"), filepath.Join(t.TempDir(), ".zot", "mcp.json"))
	enter(d) // user
	enter(d) // local command
	typeInto(d, "fs")
	enter(d)
	typeInto(d, `npx -y "@modelcontextprotocol/server-filesystem" .`)
	enter(d)
	if d.step != mcpAddExposure {
		t.Fatalf("stdio did not skip token step: %v", d.step)
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if d.step != mcpAddTarget || !strings.HasPrefix(d.input, "npx") {
		t.Fatal("back did not restore the command")
	}
	enter(d)
	enter(d) // deferred
	result, done := enter(d)
	want := mcpAddResult{Path: filepath.Join(home, "mcp.json"), Name: "fs", Config: mcp.ServerConfig{
		Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "."},
	}}
	if !done || !reflect.DeepEqual(result, want) {
		t.Fatalf("result=%+v", result)
	}
	d.Open(filepath.Join(home, "mcp.json"), filepath.Join(t.TempDir(), ".zot", "mcp.json"))
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if d.Active() {
		t.Fatal("esc on first step did not close")
	}
}

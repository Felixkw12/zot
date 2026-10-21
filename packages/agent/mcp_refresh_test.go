package agent

import (
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/tools"
)

func TestMCPCatalogRefreshPreservesTelegramTools(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "synthetic")
	args := Args{CWD: t.TempDir(), Provider: "openai", Model: "gpt-4o-mini", NoSkill: true}
	resolved, err := Resolve(args, true)
	if err != nil {
		t.Fatal(err)
	}
	mgr := extensions.New(ZotHome(), args.CWD, "test", "", "", nonInteractiveExtHooks{})
	t.Cleanup(func() { mgr.Stop(time.Millisecond) })
	source := &changingMCPSource{}
	source.set("mcp__s__old")
	mgr.AddBuiltin("mcp", source)
	adapter := &extToolAdapter{mgr: mgr}
	resolved.MergeExtensionTools(adapter)
	ag := resolved.NewAgent()
	reg := ag.ToolRegistry()
	image := &tools.TelegramSendImageTool{}
	file := &tools.TelegramSendFileTool{}
	reg[image.Name()], reg[file.Name()] = image, file
	ag.SetTools(reg)
	mgr.SetOnReload(func() {
		refreshAgentToolsAndPrompt(args, nil, adapter, ag, nil)
	})
	source.set("mcp__s__new")
	mgr.NotifyBuiltinChange()
	reg = ag.ToolRegistry()
	if reg[image.Name()] != image || reg[file.Name()] != file {
		t.Fatal("MCP refresh removed the active Telegram tools or their sender")
	}
	if reg["mcp__s__old"] != nil || reg["mcp__s__new"] == nil {
		t.Fatal("MCP catalog was not refreshed")
	}
	// Disconnecting the bridge must not be undone by later refreshes.
	delete(reg, image.Name())
	delete(reg, file.Name())
	ag.SetTools(reg)
	mgr.NotifyBuiltinChange()
	reg = ag.ToolRegistry()
	if reg[image.Name()] != nil || reg[file.Name()] != nil {
		t.Fatal("refresh restored disconnected Telegram tools")
	}
}

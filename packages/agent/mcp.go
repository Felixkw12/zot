package agent

import (
	"context"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/mcp"
)

// loadBuiltinMCP runs after extension discovery so existing bridge
// installations take precedence without opening duplicate connections.
func loadBuiltinMCP(ctx context.Context, mgr *extensions.Manager, args Args, cwd string) []error {
	if args.NoMCP || args.NoTools {
		return nil
	}
	source := mcp.New(ZotHome(), cwd)
	// Tool-list changes and on-demand reconnects update the live agent.
	source.OnChange(mgr.NotifyBuiltinChange)
	mgr.AddBuiltin("mcp", source)
	for _, extension := range mgr.All() {
		if extension.Manifest.Name == "mcp" {
			return nil
		}
	}
	return source.Reload(ctx)
}

// moveBuiltinMCP points built-in MCP at a new working directory and
// reconnects, so the project's .zot/mcp.json applies after /cd. The reload
// is serialized with /mcp reload and shutdown, and refreshes the live tool
// registry. An installed bridge extension or an opt-out leaves nothing to
// move.
func moveBuiltinMCP(ctx context.Context, mgr *extensions.Manager, cwd string) []error {
	if _, ok := mgr.Builtin("mcp").(*mcp.Manager); !ok {
		return nil
	}
	return mgr.ReloadBuiltin(ctx, "mcp", func(source extensions.Builtin) {
		if m, ok := source.(*mcp.Manager); ok {
			m.SetCWD(cwd)
		}
	})
}

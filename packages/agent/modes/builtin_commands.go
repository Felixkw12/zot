package modes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/mcp"

	"github.com/patriceckhart/zot/packages/tui"
)

func (i *Interactive) isBuiltinCommand(name string) bool {
	if i.cfg.Extensions == nil {
		return false
	}
	for _, command := range i.cfg.Extensions.Commands() {
		if strings.EqualFold(command.Name, name) {
			return command.Builtin
		}
	}
	return false
}

func renderBuiltinCommand(th tui.Theme, width int, name, text string) []string {
	if width < 20 {
		width = 20
	}
	out := []string{frameHeader(th, strings.ToUpper(name), width), ""}
	for _, line := range strings.Split(text, "\n") {
		for _, wrapped := range tui.WrapANSILine(line, width) {
			out = append(out, th.FG256(th.Muted, wrapped))
		}
	}
	return append(out, "", frameRule(th, width), "")
}

// mcpSource returns the built-in MCP manager when it is active.
func (i *Interactive) mcpSource() *mcp.Manager {
	if i.cfg.Extensions == nil {
		return nil
	}
	source, _ := i.cfg.Extensions.Builtin("mcp").(*mcp.Manager)
	return source
}

// openMCPManager opens the interactive manager for bare /mcp and /mcp add.
// Other arguments, such as reload and help, keep their text behavior.
func (i *Interactive) openMCPManager(args string) bool {
	args = strings.ToLower(strings.TrimSpace(args))
	source := i.mcpSource()
	if source == nil || (args != "" && args != "add") {
		return false
	}
	i.mcpDialog.Open(source.Snapshot, args == "add")
	i.mu.Lock()
	i.helpBlock = nil
	i.statusOK = ""
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
	return true
}

// runMCPAction performs a manager action off the input loop. Config edits
// are saved first, then MCP reloads so the live tool registry matches the
// file. A reload cancels the active turn, as /mcp reload does.
func (i *Interactive) runMCPAction(action mcpAction) {
	switch action.Kind {
	case mcpActionReconnect, mcpActionSignIn, mcpActionSignOut, mcpActionSetEnabled, mcpActionSetExposure:
		i.runMCPServerAction(action)
		return
	}
	var err error
	notice := ""
	switch action.Kind {
	case mcpActionAdd:
		err = mcp.AddServer(action.Add.Path, action.Add.Name, action.Add.Config)
		notice = "added " + action.Name + " to " + friendlyPath(action.Add.Path)
	case mcpActionRemove:
		err = mcp.RemoveServer(action.Path, action.Name)
		notice = "removed " + action.Name
	case mcpActionReload:
		notice = "reloaded MCP servers"
		if action.Name != "" {
			notice = "reconnected " + action.Name
		}
	}
	if err == nil {
		i.cancelAndWaitForIdle()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		// Per-server failures are read from the snapshot below. The joined
		// reload error only repeats them and may name unrelated servers.
		// Errors that stopped the reload itself are reported directly,
		// otherwise the unchanged snapshot would look like success.
		_, reloadErr := i.cfg.Extensions.Invoke(ctx, "mcp", "reload", 2*time.Minute)
		cancel()
		err = mcpOutcome(i.mcpSource(), action)
		if err == nil && reloadAborted(reloadErr) {
			err = errors.New("saved, but MCP did not reload: " + reloadErr.Error())
			if action.Kind == mcpActionReload {
				err = errors.New("MCP did not reload: " + reloadErr.Error())
			}
		}
	}
	i.mcpDialog.Finish(notice, err)
	i.invalidate()
}

// runMCPServerAction changes one server and reconnects only that server,
// so other servers keep their connections. The reconnect runs through the
// host, serialized with reloads, and refreshes the agent's tools.
func (i *Interactive) runMCPServerAction(action mcpAction) {
	source := i.mcpSource()
	if source == nil {
		i.mcpDialog.Finish("", errors.New("built-in MCP is no longer active"))
		i.invalidate()
		return
	}
	var err error
	notice := "reconnected " + action.Name
	switch action.Kind {
	case mcpActionSetEnabled:
		err = mcp.SetServerEnabled(action.Path, action.Name, action.Enabled)
		notice = "disabled " + action.Name
		if action.Enabled {
			notice = "enabled " + action.Name
		}
	case mcpActionSetExposure:
		err = mcp.SetServerExposure(action.Path, action.Name, action.Value)
		notice = "exposure of " + action.Name + " set to " + action.Value
	case mcpActionSignOut:
		err = source.SignOut(action.Name)
		notice = "signed out of " + action.Name
	case mcpActionSignIn:
		// The browser step can take minutes. esc cancels it.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		i.mcpDialog.SetCancel(cancel)
		err = source.SignIn(ctx, action.Name, func(status mcp.SignInStatus) {
			i.mcpDialog.SetAuthURL(status.URL, status.Paste)
			i.invalidate()
		})
		cancel()
		i.mcpDialog.SetCancel(nil)
		if errors.Is(err, context.Canceled) {
			err = errors.New("sign-in cancelled")
		} else if errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("sign-in timed out")
		}
		notice = "signed in to " + action.Name
	}
	if err == nil {
		i.cancelAndWaitForIdle()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		reloadErr := i.cfg.Extensions.UpdateBuiltin(ctx, "mcp", func(extensions.Builtin) error {
			return source.Reconnect(ctx, action.Name)
		})
		cancel()
		err = mcpOutcome(source, action)
		if err == nil && reloadAborted(reloadErr) {
			err = errors.New("saved, but the server did not reconnect: " + reloadErr.Error())
		}
	}
	i.mcpDialog.Finish(notice, err)
	i.invalidate()
}

// reloadAborted reports errors that prevented the reload from running, as
// opposed to the per-server errors a completed reload returns.
func reloadAborted(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	text := err.Error()
	return strings.Contains(text, "shutting down") || strings.Contains(text, "is unavailable")
}

// mcpOutcome reports a failure of the server an action targeted, or how
// many servers failed after a full reload.
func mcpOutcome(source *mcp.Manager, action mcpAction) error {
	if source == nil {
		return errors.New("built-in MCP is no longer active")
	}
	failed := 0
	for _, s := range source.Snapshot().Servers {
		if s.State != mcp.StateFailed && s.State != mcp.StateInvalid && s.State != mcp.StateSignIn {
			continue
		}
		// After a sign-out, needing a sign-in is the expected result.
		if action.Kind == mcpActionSignOut && s.State == mcp.StateSignIn {
			continue
		}
		if action.Name != "" && s.Name == action.Name && action.Kind != mcpActionRemove {
			return errors.New(s.Name + ": " + s.Problem)
		}
		failed++
	}
	if action.Kind == mcpActionReload && action.Name == "" && failed > 0 {
		return fmt.Errorf("%d of the servers could not be loaded, see the list", failed)
	}
	return nil
}

func (i *Interactive) showBuiltinCommand(name, text string) {
	width := 80
	if i.cfg.Terminal != nil {
		width = i.lastCols()
	}
	block := renderBuiltinCommand(i.cfg.Theme, width, name, text)
	i.mu.Lock()
	i.helpBlock = block
	i.statusOK = ""
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

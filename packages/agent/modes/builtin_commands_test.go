package modes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/mcp"
	"github.com/patriceckhart/zot/packages/tui"
)

type builtinCommandFixture struct {
	reload func()
	err    error
}

func (*builtinCommandFixture) Tools() []core.Tool { return nil }
func (b *builtinCommandFixture) Reload(context.Context) []error {
	if b.reload != nil {
		b.reload()
	}
	return nil
}
func (b *builtinCommandFixture) Command(context.Context, string) (string, error) {
	return "MCP servers\nno servers configured\nRun /mcp reload to apply changes.", b.err
}
func (*builtinCommandFixture) Close() {}

func builtinCommandInteractive(t *testing.T, source *builtinCommandFixture) *Interactive {
	t.Helper()
	mgr := extensions.New(t.TempDir(), t.TempDir(), "test", "", "", nil)
	mgr.AddBuiltin("mcp", source)
	t.Cleanup(func() { mgr.Stop(time.Millisecond) })
	return &Interactive{cfg: InteractiveConfig{Extensions: mgr}, confirmDialog: &confirmDialog{}, mcpDialog: newMCPManagerDialog()}
}

func TestBuiltinCommandUsesNativePresentation(t *testing.T) {
	i := builtinCommandInteractive(t, &builtinCommandFixture{})
	i.invokeExtensionCommand(context.Background(), "mcp", "")
	rendered := strings.Join(i.helpBlock, "\n")
	if len(i.extNotes) != 0 || !strings.Contains(rendered, "MCP") || !strings.Contains(rendered, "no servers configured") || strings.Contains(rendered, "[mcp]") || strings.Contains(rendered, "/reload-ext") {
		t.Fatalf("native block=%q extension notes=%v", rendered, i.extNotes)
	}
	i.applyExtensionCommandResponse("custom", extproto.CommandResponseFromExt{Action: "display", Display: "extension note"})
	if len(i.extNotes) != 1 || !strings.Contains(i.extNotes[0], "[custom]") {
		t.Fatalf("extension presentation changed: %v", i.extNotes)
	}
}

func TestBuiltinReloadCancelsTurnBeforeChangingTools(t *testing.T) {
	source := &builtinCommandFixture{}
	i := builtinCommandInteractive(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	i.busy = true
	i.cancelTurn = func() { cancel(); i.mu.Lock(); i.busy = false; i.mu.Unlock() }
	reloaded := false
	source.reload = func() {
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("reload ran before the active turn was cancelled")
		}
		reloaded = true
	}
	i.invokeExtensionCommand(context.Background(), "mcp", "reload")
	if !reloaded {
		t.Fatal("native reload did not run")
	}
	if i.statusErr != "" {
		t.Fatal(i.statusErr)
	}
}

func mcpManagerInteractive(t *testing.T) (*Interactive, string) {
	t.Helper()
	home, cwd := t.TempDir(), t.TempDir()
	mgr := extensions.New(home, cwd, "test", "", "", nil)
	source := mcp.New(home, cwd)
	mgr.AddBuiltin("mcp", source)
	t.Cleanup(func() { mgr.Stop(time.Millisecond) })
	if errs := source.Reload(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	return &Interactive{cfg: InteractiveConfig{Extensions: mgr, ZotHome: home, CWD: cwd}, confirmDialog: &confirmDialog{}, mcpDialog: newMCPManagerDialog()}, home
}

func TestMCPManagerOpensForBareAndAddOnly(t *testing.T) {
	i, _ := mcpManagerInteractive(t)
	i.invokeExtensionCommand(context.Background(), "mcp", "")
	if !i.mcpDialog.Active() || i.mcpDialog.view != mcpViewList {
		t.Fatal("/mcp did not open the manager list")
	}
	i.mcpDialog.Close()
	i.invokeExtensionCommand(context.Background(), "mcp", "add")
	if i.mcpDialog.view != mcpViewAdd {
		t.Fatal("/mcp add did not open the add view")
	}
	i.mcpDialog.Close()
	i.invokeExtensionCommand(context.Background(), "mcp", "help")
	if i.mcpDialog.Active() || len(i.helpBlock) == 0 {
		t.Fatal("/mcp help should keep the text block")
	}
}

func TestMCPManagerActionsEditConfigAndReload(t *testing.T) {
	i, home := mcpManagerInteractive(t)
	path := filepath.Join(home, "mcp.json")
	i.openMCPManager("")
	run := func(action mcpAction) {
		t.Helper()
		if action.Kind == mcpActionNone {
			t.Fatal("expected an action")
		}
		i.runMCPAction(action)
	}
	run(mcpAction{Kind: mcpActionAdd, Name: "fs", Add: mcpAddResult{Path: path, Name: "fs", Config: mcp.ServerConfig{Command: "zot-mcp-test-missing-binary"}}})
	snap := i.mcpSource().Snapshot()
	if len(snap.Servers) != 1 || snap.Servers[0].State != mcp.StateFailed || snap.Servers[0].Problem != "command not found" {
		t.Fatalf("snapshot=%+v", snap)
	}
	if i.mcpDialog.err != "fs: command not found" {
		t.Fatalf("outcome=%q", i.mcpDialog.err)
	}
	i.mcpDialog.selected = "fs"
	run(i.mcpDialog.shortcutLocked(tui.Key{Kind: tui.KeyRune, Rune: 'e'}, &snap.Servers[0]))
	snap = i.mcpSource().Snapshot()
	if snap.Servers[0].State != mcp.StateDisabled || i.mcpDialog.notice != "disabled fs" {
		t.Fatalf("disable: %+v notice=%q err=%q", snap.Servers[0], i.mcpDialog.notice, i.mcpDialog.err)
	}
	run(mcpAction{Kind: mcpActionSetExposure, Name: "fs", Path: path, Value: "direct"})
	if snap = i.mcpSource().Snapshot(); snap.Servers[0].Exposure != "direct" {
		t.Fatalf("exposure: %+v", snap.Servers[0])
	}
	run(mcpAction{Kind: mcpActionRemove, Name: "fs", Path: path})
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "fs") || len(i.mcpSource().Snapshot().Servers) != 0 {
		t.Fatalf("remove left %s", data)
	}
}

func TestMCPManagerEnableInvalidServerReportsFailure(t *testing.T) {
	i, home := mcpManagerInteractive(t)
	path := filepath.Join(home, "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"s":{"enabled":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	i.runMCPAction(mcpAction{Kind: mcpActionReload})
	i.runMCPAction(mcpAction{Kind: mcpActionSetEnabled, Name: "s", Path: path, Enabled: true})
	if i.mcpDialog.notice != "" || !strings.Contains(i.mcpDialog.err, "command") {
		t.Fatalf("enable result: notice=%q err=%q", i.mcpDialog.notice, i.mcpDialog.err)
	}
	if snap := i.mcpSource().Snapshot(); len(snap.Servers) != 1 || snap.Servers[0].State != mcp.StateInvalid {
		t.Fatalf("enable snapshot: %+v", snap)
	}
}

func TestMCPManagerReconnectMissingServerReportsFailure(t *testing.T) {
	i, _ := mcpManagerInteractive(t)
	i.runMCPAction(mcpAction{Kind: mcpActionReconnect, Name: "missing"})
	if i.mcpDialog.notice != "" || !strings.Contains(i.mcpDialog.err, "not configured") {
		t.Fatalf("reconnect result: notice=%q err=%q", i.mcpDialog.notice, i.mcpDialog.err)
	}
}

func TestBuiltinCommandErrorsAreNotLabelledAsExtensions(t *testing.T) {
	i := builtinCommandInteractive(t, &builtinCommandFixture{err: errors.New("failed")})
	i.invokeExtensionCommand(context.Background(), "mcp", "")
	if i.statusErr != "/mcp: failed" {
		t.Fatalf("status=%q", i.statusErr)
	}
}

func TestMCPManagerReconnectKeepsOtherServers(t *testing.T) {
	i, home := mcpManagerInteractive(t)
	path := filepath.Join(home, "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"a":{"enabled":false},"b":{"command":"zot-mcp-test-missing-binary"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	refreshes := 0
	i.cfg.Extensions.SetOnReload(func() { refreshes++ })
	i.runMCPAction(mcpAction{Kind: mcpActionReload})
	refreshes = 0
	i.openMCPManager("")
	i.runMCPAction(mcpAction{Kind: mcpActionReconnect, Name: "b"})
	if refreshes != 1 || i.mcpDialog.err != "b: command not found" {
		t.Fatalf("refreshes=%d err=%q", refreshes, i.mcpDialog.err)
	}
	response, err := i.cfg.Extensions.Invoke(context.Background(), "mcp", "reconnect b", time.Second)
	if err != nil || !strings.Contains(response.Display, "b: failed") || refreshes != 2 {
		t.Fatalf("text reconnect: %+v %v refreshes=%d", response, err, refreshes)
	}
	if _, err := i.cfg.Extensions.Invoke(context.Background(), "mcp", "bogus x", time.Second); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
}

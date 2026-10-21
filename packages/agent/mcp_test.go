package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/mcp"
)

func TestBuiltinMCPReloadReadsConfigAndRefreshesHost(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	path := filepath.Join(ZotHome(), "mcp.json")
	writeConfig := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(`{"mcpServers":{"old":{"enabled":false}}}`)
	mgr := extensions.New(ZotHome(), t.TempDir(), "test", "", "", nonInteractiveExtHooks{})
	t.Cleanup(func() { mgr.Stop(time.Millisecond) })
	if errs := loadBuiltinMCP(context.Background(), mgr, Args{}, t.TempDir()); len(errs) != 0 {
		t.Fatal(errs)
	}
	refreshes := 0
	mgr.SetOnReload(func() { refreshes++ })
	writeConfig(`{"mcpServers":{"new":{"enabled":false}}}`)
	response, err := mgr.Invoke(context.Background(), "mcp", "reload", time.Second)
	if err != nil || refreshes != 1 || !strings.Contains(response.Display, "new: disabled") || strings.Contains(response.Display, "old: disabled") {
		t.Fatalf("response=%+v err=%v refreshes=%d", response, err, refreshes)
	}
	writeConfig(`{"mcpServers":`)
	if _, err := mgr.Invoke(context.Background(), "mcp", "reload", time.Second); err == nil {
		t.Fatal("invalid config reload succeeded")
	}
	if refreshes != 2 {
		t.Fatalf("failed reload did not refresh host: %d", refreshes)
	}
}

func TestBuiltinMCPLoadAndOptOut(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(ZotHome(), "mcp.json"), []byte(`{"mcpServers":{"local":{"enabled":false}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range []Args{{}, {NoExt: true}, {NoMCP: true}, {NoTools: true}} {
		mgr := extensions.New(ZotHome(), t.TempDir(), "test", "", "", nonInteractiveExtHooks{})
		t.Cleanup(func() { mgr.Stop(time.Millisecond) })
		if errs := loadBuiltinMCP(context.Background(), mgr, args, t.TempDir()); len(errs) != 0 {
			t.Fatal(errs)
		}
		if args.NoMCP || args.NoTools {
			if mgr.HasCommand("mcp") {
				t.Fatal("opt-out still loaded MCP")
			}
			continue
		}
		if !mgr.HasCommand("mcp") {
			t.Fatal("native MCP command missing")
		}
		response, err := mgr.Invoke(context.Background(), "mcp", "", time.Second)
		if err != nil || !strings.Contains(response.Display, "local: disabled") {
			t.Fatalf("response=%+v err=%v", response, err)
		}
	}
}

func TestBuiltinMCPFollowsWorkingDirectoryChange(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	first, second := t.TempDir(), t.TempDir()
	for dir, name := range map[string]string{first: "alpha", second: "beta"} {
		if err := os.MkdirAll(filepath.Join(dir, ".zot"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".zot", "mcp.json"), []byte(`{"mcpServers":{"`+name+`":{"enabled":false}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mgr := extensions.New(ZotHome(), first, "test", "", "", nonInteractiveExtHooks{})
	t.Cleanup(func() { mgr.Stop(time.Millisecond) })
	if errs := loadBuiltinMCP(context.Background(), mgr, Args{}, first); len(errs) != 0 {
		t.Fatal(errs)
	}
	refreshes := 0
	mgr.SetOnReload(func() { refreshes++ })
	if errs := moveBuiltinMCP(context.Background(), mgr, second); len(errs) != 0 {
		t.Fatal(errs)
	}
	snap := mgr.Builtin("mcp").(*mcp.Manager).Snapshot()
	if len(snap.Servers) != 1 || snap.Servers[0].Name != "beta" || snap.ProjectPath != filepath.Join(second, ".zot", "mcp.json") || refreshes != 1 {
		t.Fatalf("snapshot=%+v refreshes=%d", snap, refreshes)
	}
	off := extensions.New(ZotHome(), first, "test", "", "", nonInteractiveExtHooks{})
	t.Cleanup(func() { off.Stop(time.Millisecond) })
	if errs := moveBuiltinMCP(context.Background(), off, second); len(errs) != 0 {
		t.Fatal(errs)
	}
}

func TestBuiltinMCPChangesRefreshTheHost(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(ZotHome(), "mcp.json"), []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := extensions.New(ZotHome(), t.TempDir(), "test", "", "", nonInteractiveExtHooks{})
	t.Cleanup(func() { mgr.Stop(time.Millisecond) })
	if errs := loadBuiltinMCP(context.Background(), mgr, Args{}, t.TempDir()); len(errs) != 0 {
		t.Fatal(errs)
	}
	refreshes := 0
	mgr.SetOnReload(func() { refreshes++ })
	mgr.NotifyBuiltinChange()
	if refreshes != 1 {
		t.Fatalf("change did not refresh the host: %d", refreshes)
	}
	mgr.Stop(time.Millisecond)
	mgr.NotifyBuiltinChange()
	if refreshes != 1 {
		t.Fatal("refresh ran after shutdown")
	}
}

// toolStub is a minimal built-in tool for registry tests.
type toolStub struct{ name string }

func (t toolStub) Name() string            { return t.name }
func (t toolStub) Description() string     { return "stub" }
func (t toolStub) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t toolStub) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	return core.ToolResult{}, nil
}

func TestNonInteractiveAgentFollowsMCPChanges(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	cwd := t.TempDir()
	path := filepath.Join(ZotHome(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := extensions.New(ZotHome(), cwd, "test", "", "", nonInteractiveExtHooks{})
	t.Cleanup(func() { mgr.Stop(time.Millisecond) })
	if errs := loadBuiltinMCP(context.Background(), mgr, Args{}, cwd); len(errs) != 0 {
		t.Fatal(errs)
	}
	ag := core.NewAgent(nil, "test", "", core.NewRegistry(toolStub{"read"}))
	wireNonInteractiveAgentExtHooks(context.Background(), ag, mgr)
	server := mcpFixtureServer(t)
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"docs":{"url":"`+server+`","exposure":"direct"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Invoke(context.Background(), "mcp", "reload", time.Second); err != nil {
		t.Fatal(err)
	}
	reg := ag.ToolRegistry()
	if _, ok := reg["read"]; !ok {
		t.Fatal("built-in tool lost")
	}
	if _, ok := reg["mcp__docs__lookup"]; !ok {
		t.Fatalf("MCP tool not added: %v", reg)
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Invoke(context.Background(), "mcp", "reload", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok := ag.ToolRegistry()["mcp__docs__lookup"]; ok {
		t.Fatal("removed MCP tool still registered")
	}
}

// mcpFixtureServer serves one MCP tool named lookup over streamable HTTP.
func mcpFixtureServer(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "f", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "lookup", "inputSchema": map[string]any{"type": "object"}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

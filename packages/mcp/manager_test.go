package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name   string `json:"name"`
		Cursor string `json:"cursor"`
	} `json:"params"`
}

func fixtureResult(req rpcRequest) any {
	switch req.Method {
	case "initialize":
		return map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
	case "tools/list":
		name := "repo.lookup"
		result := map[string]any{}
		if req.Params.Cursor == "" {
			result["nextCursor"] = "next"
		} else {
			name = "issues"
		}
		result["tools"] = []any{map[string]any{"name": name, "description": "Search repository metadata", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}
		return result
	case "tools/call":
		return map[string]any{"content": []any{map[string]string{"type": "text", "text": "called " + req.Params.Name}}, "structuredContent": map[string]bool{"ok": true}, "isError": req.Params.Name == "issues"}
	}
	return nil
}

func fixtureHTTP(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == "tools/call" {
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": fixtureResult(req)})
	}))
	t.Cleanup(server.Close)
	return server, calls
}

func httpManager(t *testing.T, extra string) (*Manager, *atomic.Int32) {
	t.Helper()
	server, calls := fixtureHTTP(t)
	home, project := t.TempDir(), t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), fmt.Sprintf(`{"mcpServers":{"docs":{"url":%q,"headers":{"Authorization":"Bearer synthetic"}%s}}}`, server.URL, extra))
	m := New(home, project)
	t.Cleanup(m.Close)
	if errs := m.Reload(context.Background()); len(errs) > 0 {
		t.Fatal(errs)
	}
	return m, calls
}

func registry(m *Manager) core.Registry { return core.NewRegistry(m.Tools()...) }

func TestHTTPDiscoveryPaginationSearchAndResults(t *testing.T) {
	m, calls := httpManager(t, "")
	reg := registry(m)
	if len(reg) != 3 {
		t.Fatalf("tools=%v", reg)
	}
	for _, spec := range reg.AllSpecs() {
		if spec.Name != "mcp__search_tools" && !spec.Deferred {
			t.Fatalf("tool not deferred: %+v", spec)
		}
	}
	search := reg["mcp__search_tools"]
	result, err := search.Execute(context.Background(), json.RawMessage(`{"query":"repository","limit":1}`), nil)
	if err != nil || !reflect.DeepEqual(result.ActivateTools, []string{"mcp__docs__issues"}) {
		t.Fatalf("search=%+v err=%v", result, err)
	}
	if calls.Load() != 0 {
		t.Fatal("search executed a remote tool")
	}
	tool := reg["mcp__docs__repo_lookup"]
	result, err = tool.Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || result.IsError || string(result.StructuredContent) != `{"ok":true}` || result.Content[0].(provider.TextBlock).Text != "called repo.lookup" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	result, err = reg["mcp__docs__issues"].Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || !result.IsError {
		t.Fatalf("remote error lost: %+v %v", result, err)
	}
	for _, raw := range []string{`{}`, `{"query":" "}`, `{"query":"absent"}`} {
		result, err = search.Execute(context.Background(), json.RawMessage(raw), nil)
		if err != nil || len(result.ActivateTools) != 0 {
			t.Fatalf("search %s=%+v %v", raw, result, err)
		}
	}
	if _, err = search.Execute(context.Background(), json.RawMessage(`{"query":"x","limit":21}`), nil); err == nil {
		t.Fatal("invalid limit accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = tool.Execute(ctx, json.RawMessage(`{}`), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("remote calls=%d", calls.Load())
	}
	if _, err = tool.Execute(context.Background(), json.RawMessage(`[]`), nil); err == nil {
		t.Fatal("array arguments accepted")
	}
	m.Close()
	if _, err = tool.Execute(context.Background(), json.RawMessage(`{}`), nil); err == nil {
		t.Fatal("closed connection callable")
	}
}

func TestExposureAndReloadWithdrawsTools(t *testing.T) {
	m, _ := httpManager(t, `,"exposure":"direct"`)
	for _, spec := range registry(m).AllSpecs() {
		if spec.Deferred {
			t.Fatal("direct tool deferred")
		}
	}
	writeConfig(t, filepath.Join(m.home, "mcp.json"), `{"mcpServers":{"docs":{"enabled":false}}}`)
	if errs := m.Reload(context.Background()); len(errs) > 0 {
		t.Fatal(errs)
	}
	if len(m.Tools()) != 0 {
		t.Fatal("withdrawn tools retained")
	}
	status, err := m.Command(context.Background(), "")
	if err != nil || !strings.Contains(status, "disabled") {
		t.Fatalf("status=%s err=%v", status, err)
	}
	hidden, _ := httpManager(t, `,"exposure":"hidden"`)
	if len(hidden.Tools()) != 0 {
		t.Fatal("hidden tools reachable")
	}
}

// TestMCPStdioHelper is a subprocess fixture, never an external MCP service.
func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv("ZOT_MCP_HELPER") != "1" {
		return
	}
	reader := bufio.NewScanner(os.Stdin)
	writer := json.NewEncoder(os.Stdout)
	for reader.Scan() {
		var req rpcRequest
		if json.Unmarshal(reader.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		result := fixtureResult(req)
		if req.Method == "tools/call" {
			cwd, _ := os.Getwd()
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": cwd + "|" + os.Getenv("ZOT_MCP_VALUE")}}}
		}
		_ = writer.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	os.Exit(0)
}

func TestStdioCWDEnvironmentAndCleanup(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home, project := t.TempDir(), t.TempDir()
	config := map[string]any{"mcpServers": map[string]any{"local": map[string]any{"command": binary, "args": []string{"-test.run=^TestMCPStdioHelper$"}, "env": map[string]string{"ZOT_MCP_HELPER": "1", "ZOT_MCP_VALUE": "synthetic"}}}}
	data, _ := json.Marshal(config)
	writeConfig(t, filepath.Join(home, "mcp.json"), string(data))
	m := New(home, project)
	t.Cleanup(m.Close)
	if errs := m.Reload(context.Background()); len(errs) > 0 {
		t.Fatal(errs)
	}
	canonicalProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry(m)["mcp__local__repo_lookup"].Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || result.Content[0].(provider.TextBlock).Text != canonicalProject+"|synthetic" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	process := m.servers["local"].process
	m.Close()
	if process.ProcessState == nil {
		t.Fatal("subprocess not reaped")
	}
}

func TestSnapshotReportsStateSourceAndToolsWithoutSecrets(t *testing.T) {
	server, _ := fixtureHTTP(t)
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("ZOT_MCP_SNAPSHOT_TOKEN", "synthetic")
	writeConfig(t, filepath.Join(home, "mcp.json"), fmt.Sprintf(`{"mcpServers":{
 "docs":{"url":%q,"headers":{"Authorization":"Bearer ${ZOT_MCP_SNAPSHOT_TOKEN}"}},
 "shadowed":{"command":"user-version"},
 "off":{"command":"x","enabled":false,"exposure":"hidden"}}}`, server.URL+"/?token=${ZOT_MCP_SNAPSHOT_TOKEN}"))
	writeConfig(t, filepath.Join(project, ".zot", "mcp.json"), `{"mcpServers":{
 "shadowed":{"command":"zot-mcp-test-missing-binary"},
 "broken":{"command":"x","exposure":"loud"}}}`)
	m := New(home, project)
	t.Cleanup(m.Close)
	if snap := m.Snapshot(); snap.Loaded {
		t.Fatal("snapshot loaded before Reload")
	}
	m.Reload(context.Background())
	snap := m.Snapshot()
	got := map[string]ServerInfo{}
	var order []string
	for _, s := range snap.Servers {
		got[s.Name] = s
		order = append(order, s.Name)
	}
	if !reflect.DeepEqual(order, []string{"broken", "shadowed", "docs", "off"}) {
		t.Fatalf("order=%v", order)
	}
	docs := got["docs"]
	if docs.State != StateConnected || docs.ToolCount != 2 || len(docs.Tools) != 2 || docs.Project || docs.Transport != "streamable-http" {
		t.Fatalf("docs=%+v", docs)
	}
	if strings.Contains(docs.Target, "synthetic") || !strings.Contains(docs.Target, "${ZOT_MCP_SNAPSHOT_TOKEN}") {
		t.Fatalf("target exposes the expanded secret: %q", docs.Target)
	}
	if s := got["shadowed"]; s.State != StateFailed || s.Problem != "command not found" || !s.Project || !s.Override {
		t.Fatalf("shadowed=%+v", s)
	}
	if s := got["broken"]; s.State != StateInvalid || !strings.Contains(s.Problem, "exposure") || !s.Project {
		t.Fatalf("broken=%+v", s)
	}
	if s := got["off"]; s.State != StateDisabled || s.Exposure != "hidden" || s.Transport != "stdio" {
		t.Fatalf("off=%+v", s)
	}
	writeConfig(t, filepath.Join(project, ".zot", "mcp.json"), `{`)
	m.Reload(context.Background())
	if snap = m.Snapshot(); len(snap.FileErrors) != 1 || !strings.Contains(snap.FileErrors[0], "invalid JSON") {
		t.Fatalf("file errors=%v", snap.FileErrors)
	}
}

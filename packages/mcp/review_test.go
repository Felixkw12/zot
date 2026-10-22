package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// schemaServer advertises one tool with the given input schema.
func schemaServer(t *testing.T, schema any, cursors func(string) (string, bool)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		result := fixtureResult(req)
		if req.Method == "tools/list" {
			page := map[string]any{"tools": []any{map[string]any{"name": "t" + req.Params.Cursor, "inputSchema": schema}}}
			if cursors != nil {
				if next, ok := cursors(req.Params.Cursor); ok {
					page["nextCursor"] = next
				}
			}
			result = page
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func loadOne(t *testing.T, url string) (*Manager, []error) {
	t.Helper()
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), fmt.Sprintf(`{"mcpServers":{"s":{"url":%q,"exposure":"direct","connectTimeout":5}}}`, url))
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	return m, m.Reload(context.Background())
}

// The typed schema of the client library drops composition keywords and
// renames definitions, which would break $ref pointers sent to the model.
func TestToolSchemaIsForwardedVerbatim(t *testing.T) {
	schema := map[string]any{
		"type": "object", "description": "top", "definitions": map[string]any{"P": map[string]any{"type": "string"}},
		"properties": map[string]any{"p": map[string]any{"$ref": "#/definitions/P"}},
		"anyOf":      []any{map[string]any{"required": []string{"p"}}},
	}
	m, errs := loadOne(t, schemaServer(t, schema, nil).URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	var got map[string]any
	if err := json.Unmarshal(core.NewRegistry(m.Tools()...)["mcp__s__t"].Schema(), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"anyOf", "definitions", "description"} {
		if got[key] == nil {
			t.Fatalf("schema lost %q: %v", key, got)
		}
	}
}

func TestMissingSchemaBecomesEmptyObject(t *testing.T) {
	m, errs := loadOne(t, schemaServer(t, nil, nil).URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if got := string(core.NewRegistry(m.Tools()...)["mcp__s__t"].Schema()); got != `{"type":"object","properties":{}}` {
		t.Fatalf("schema=%s", got)
	}
}

// Providers reject tools whose input schema is not an object, which would
// fail every request while such a tool is advertised.
func TestNonObjectSchemaBecomesEmptyObject(t *testing.T) {
	for _, schema := range []any{map[string]any{"type": "array"}, map[string]any{"type": "string"}, map[string]any{"type": 7}} {
		m, errs := loadOne(t, schemaServer(t, schema, nil).URL)
		if len(errs) != 0 {
			t.Fatal(errs)
		}
		if got := string(core.NewRegistry(m.Tools()...)["mcp__s__t"].Schema()); got != `{"type":"object","properties":{}}` {
			t.Fatalf("schema %v forwarded as %s", schema, got)
		}
	}
	if got := string(toolSchema(json.RawMessage(`{"type":["object","null"],"properties":{}}`))); !strings.Contains(got, `"null"`) {
		t.Fatalf("object union replaced: %s", got)
	}
}

// A tool result larger than the inline limit is cut and saved whole, so
// one call cannot fill the context window.
func TestLargeToolResultIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		result := fixtureResult(req)
		if req.Method == "tools/call" {
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": strings.Repeat("x", maxInlineTextSize+1)}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	m, errs := loadOne(t, srv.URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	result, err := core.NewRegistry(m.Tools()...)["mcp__s__issues"].Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(provider.TextBlock).Text
	if len(text) > maxInlineTextSize+256 || !strings.Contains(text, "from tool mcp__s__issues, complete text saved to ") {
		t.Fatalf("len=%d tail=%q", len(text), text[len(text)-120:])
	}
	path := strings.TrimSuffix(text[strings.LastIndex(text, "saved to ")+len("saved to "):], "]")
	if data, err := os.ReadFile(filepath.FromSlash(path)); err != nil || len(data) != maxInlineTextSize+1 {
		t.Fatalf("saved file: %d bytes %v", len(data), err)
	}
	m.Close()
	if _, err := os.Stat(filepath.FromSlash(path)); !os.IsNotExist(err) {
		t.Fatalf("file survived Close: %v", err)
	}
}

func TestToolPaginationStopsOnRepeatedCursor(t *testing.T) {
	loop := func(cursor string) (string, bool) { return "again", true }
	done := make(chan []error, 1)
	go func() {
		_, errs := loadOne(t, schemaServer(t, map[string]any{"type": "object"}, loop).URL)
		done <- errs
	}()
	select {
	case errs := <-done:
		if len(errs) != 1 {
			t.Fatalf("errors=%v", errs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("discovery did not stop on a repeated cursor")
	}
	pages := func(cursor string) (string, bool) {
		if cursor == "" {
			return "2", true
		}
		return "", false
	}
	m, errs := loadOne(t, schemaServer(t, map[string]any{"type": "object"}, pages).URL)
	if len(errs) != 0 || len(m.Tools()) != 3 {
		t.Fatalf("errors=%v tools=%d", errs, len(m.Tools()))
	}
}

func TestCrashedStdioServerIsReportedAndNotCalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses process groups")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	config := map[string]any{"mcpServers": map[string]any{"local": map[string]any{"command": binary, "args": []string{"-test.run=^TestMCPStdioHelper$"}, "env": map[string]string{"ZOT_MCP_HELPER": "1"}}}}
	data, _ := json.Marshal(config)
	writeConfig(t, filepath.Join(home, "mcp.json"), string(data))
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	if errs := m.Reload(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	terminateProcess(m.servers["local"].process)
	deadline := time.Now().Add(5 * time.Second)
	for m.Snapshot().Servers[0].State != StateFailed {
		if time.Now().After(deadline) {
			t.Fatal("crashed server still reported as connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The next call reconnects the server instead of failing.
	changed := make(chan struct{}, 1)
	m.OnChange(func() { changed <- struct{}{} })
	result, err := registry(m)["mcp__local__repo_lookup"].Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("call after crash: result=%+v err=%v", result, err)
	}
	if m.Snapshot().Servers[0].State != StateConnected {
		t.Fatalf("not reconnected: %+v", m.Snapshot().Servers)
	}
	select {
	case <-changed:
	default:
		t.Fatal("host was not told about the new connection")
	}
	m.Close()
	if s := m.Snapshot(); len(s.Servers) != 0 {
		t.Fatalf("closed manager still lists servers: %+v", s.Servers)
	}
}

func TestDisplayedURLHidesLiteralCredentials(t *testing.T) {
	got := describeTarget(ServerConfig{URL: "https://user:pass@example.test/mcp?key=literal&token=${TOKEN}"})
	if strings.Contains(got, "pass") || strings.Contains(got, "literal") || !strings.Contains(got, "token=${TOKEN}") {
		t.Fatalf("target=%s", got)
	}
}

func TestHTTPFailuresGetSpecificHints(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusUnauthorized: "sign in to connect",
		http.StatusNotFound:     "try transport sse",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		m, errs := loadOne(t, srv.URL)
		srv.Close()
		if len(errs) != 1 {
			t.Fatalf("%d: errors=%v", status, errs)
		}
		if got := m.Snapshot().Servers[0].Problem; !strings.Contains(got, want) {
			t.Fatalf("%d: problem=%q", status, got)
		}
	}
}

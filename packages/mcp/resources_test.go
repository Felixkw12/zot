package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

var pngPixel, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")

// resourceServer serves tools and resources over streamable HTTP. Without
// resources it advertises no resource capability.
func resourceFixture(t *testing.T, withResources bool, failing *atomic.Bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
				URI    string `json:"uri"`
			} `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		reply := func(result any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		fail := func(message string) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32002, "message": message}})
		}
		if failing != nil && failing.Load() && strings.HasPrefix(req.Method, "resources/") {
			fail("resources unavailable")
			return
		}
		switch req.Method {
		case "initialize":
			capabilities := map[string]any{"tools": map[string]any{}}
			if withResources {
				capabilities["resources"] = map[string]any{}
			}
			reply(map[string]any{"protocolVersion": "2025-03-26", "capabilities": capabilities, "serverInfo": map[string]string{"name": "f", "version": "1"}})
		case "tools/list":
			reply(map[string]any{"tools": []any{map[string]any{"name": "lookup", "inputSchema": map[string]any{"type": "object"}}}})
		case "resources/list":
			if req.Params.Cursor == "" {
				reply(map[string]any{"resources": []any{
					map[string]any{"uri": "file:///readme.md", "name": "readme", "mimeType": "text/markdown", "icons": []any{"x"}},
					map[string]any{"uri": "ui://widget", "name": "widget"},
				}, "nextCursor": "2"})
				return
			}
			reply(map[string]any{"resources": []any{
				map[string]any{"uri": "file:///logo.png", "name": "logo", "mimeType": "image/png"},
				map[string]any{"uri": "file:///data.pdf", "name": "data", "mimeType": "application/pdf"},
			}})
		case "resources/templates/list":
			reply(map[string]any{"resourceTemplates": []any{map[string]any{"uriTemplate": "file:///{path}", "name": "file"}}})
		case "resources/read":
			switch req.Params.URI {
			case "file:///readme.md":
				reply(map[string]any{"contents": []any{map[string]any{"uri": req.Params.URI, "mimeType": "text/markdown", "text": "# Hello"}}})
			case "file:///logo.png":
				reply(map[string]any{"contents": []any{map[string]any{"uri": req.Params.URI, "mimeType": "image/png", "blob": base64.StdEncoding.EncodeToString(pngPixel)}}})
			case "file:///data.pdf":
				reply(map[string]any{"contents": []any{map[string]any{"uri": req.Params.URI, "mimeType": "application/pdf", "blob": base64.StdEncoding.EncodeToString([]byte("%PDF-1.4"))}}})
			case "file:///large.txt":
				reply(map[string]any{"contents": []any{map[string]any{"uri": req.Params.URI, "mimeType": "text/plain", "text": strings.Repeat("ä", maxInlineTextSize)}}})
			default:
				fail("resource not found: " + req.Params.URI)
			}
		default:
			reply(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func resourceManager(t *testing.T, servers map[string]string) *Manager {
	t.Helper()
	entries := map[string]any{}
	for name, entry := range servers {
		var value any
		if err := json.Unmarshal([]byte(entry), &value); err != nil {
			t.Fatal(err)
		}
		entries[name] = value
	}
	data, _ := json.Marshal(map[string]any{"mcpServers": entries})
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), string(data))
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	if errs := m.Reload(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	return m
}

func callTool(t *testing.T, m *Manager, name, args string) (core.ToolResult, error) {
	t.Helper()
	tool, ok := core.NewRegistry(m.Tools()...)[name]
	if !ok {
		t.Fatalf("tool %s not registered", name)
	}
	return tool.Execute(context.Background(), json.RawMessage(args), nil)
}

func textOf(result core.ToolResult) string {
	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(provider.TextBlock); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func TestResourceToolsListAndRead(t *testing.T) {
	srv := resourceFixture(t, true, nil)
	m := resourceManager(t, map[string]string{"docs": fmt.Sprintf(`{"url":%q}`, srv.URL)})
	all, err := callTool(t, m, listResourcesTool, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Resources []map[string]any `json:"resources"`
	}
	if err := json.Unmarshal([]byte(textOf(all)), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Resources) != 3 {
		t.Fatalf("all pages not followed or app resource kept: %s", textOf(all))
	}
	for _, item := range listed.Resources {
		if item["server"] != "docs" || item["icons"] != nil {
			t.Fatalf("item=%v", item)
		}
	}
	page, err := callTool(t, m, listResourcesTool, `{"server":"docs"}`)
	if err != nil || !strings.Contains(textOf(page), `"nextCursor": "2"`) {
		t.Fatalf("single page: %s %v", textOf(page), err)
	}
	next, err := callTool(t, m, listResourcesTool, `{"server":"docs","cursor":"2"}`)
	if err != nil || !strings.Contains(textOf(next), "logo.png") || strings.Contains(textOf(next), "nextCursor") {
		t.Fatalf("next page: %s %v", textOf(next), err)
	}
	templates, err := callTool(t, m, listTemplatesTool, `{}`)
	if err != nil || !strings.Contains(textOf(templates), "file:///{path}") {
		t.Fatalf("templates: %s %v", textOf(templates), err)
	}

	text, err := callTool(t, m, readResourceTool, `{"server":"docs","uri":"file:///readme.md"}`)
	if err != nil || textOf(text) != "# Hello" {
		t.Fatalf("text: %+v %v", text, err)
	}
	image, err := callTool(t, m, readResourceTool, `{"server":"docs","uri":"file:///logo.png"}`)
	if err != nil || len(image.Content) != 1 {
		t.Fatalf("image: %+v %v", image, err)
	}
	if block, ok := image.Content[0].(provider.ImageBlock); !ok || block.MimeType != "image/png" || len(block.Data) != len(pngPixel) {
		t.Fatalf("image block=%+v", image.Content[0])
	}
	binary, err := callTool(t, m, readResourceTool, `{"server":"docs","uri":"file:///data.pdf"}`)
	if err != nil || !strings.Contains(textOf(binary), "saved to ") {
		t.Fatalf("binary: %s %v", textOf(binary), err)
	}
	path := filepath.FromSlash(textOf(binary)[strings.LastIndex(textOf(binary), "saved to ")+len("saved to "):])
	t.Cleanup(func() { os.Remove(path) })
	if data, err := os.ReadFile(path); err != nil || string(data) != "%PDF-1.4" || !strings.HasSuffix(path, ".pdf") {
		t.Fatalf("saved file %q: %q %v", path, data, err)
	}
	if _, err := callTool(t, m, readResourceTool, `{"server":"docs","uri":"file:///missing"}`); err == nil || !strings.Contains(err.Error(), "resource not found") {
		t.Fatalf("missing resource: %v", err)
	}
	if _, err := callTool(t, m, readResourceTool, `{"server":"docs"}`); err == nil {
		t.Fatal("read without uri accepted")
	}
	if _, err := callTool(t, m, listResourcesTool, `{"cursor":"2"}`); err == nil {
		t.Fatal("cursor without server accepted")
	}
	if info := m.Snapshot().Servers[0]; !info.Resources {
		t.Fatal("snapshot does not report resources")
	}
}

// Large text is cut at the inline limit and saved whole, and files handed
// to the model by path do not outlive the manager.
func TestLargeResultsAreSpilledToFilesRemovedOnClose(t *testing.T) {
	srv := resourceFixture(t, true, nil)
	m := resourceManager(t, map[string]string{"docs": fmt.Sprintf(`{"url":%q}`, srv.URL)})
	large, err := callTool(t, m, readResourceTool, `{"server":"docs","uri":"file:///large.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	text := textOf(large)
	if len(text) > maxInlineTextSize+256 || !utf8.ValidString(text) || !strings.Contains(text, "complete text saved to ") {
		t.Fatalf("large text not bounded cleanly: len=%d valid=%v tail=%q", len(text), utf8.ValidString(text), text[len(text)-120:])
	}
	spilled := text[strings.LastIndex(text, "saved to ")+len("saved to "):]
	spilled = strings.TrimSuffix(spilled, "]")
	if data, err := os.ReadFile(filepath.FromSlash(spilled)); err != nil || len(data) != maxInlineTextSize*2 {
		t.Fatalf("spilled file %q: %d bytes %v", spilled, len(data), err)
	}
	binary, err := callTool(t, m, readResourceTool, `{"server":"docs","uri":"file:///data.pdf"}`)
	if err != nil {
		t.Fatal(err)
	}
	saved := filepath.FromSlash(textOf(binary)[strings.LastIndex(textOf(binary), "saved to ")+len("saved to "):])
	m.Close()
	for _, path := range []string{spilled, saved} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived Close: %v", path, err)
		}
	}
}

func TestResourceToolsOnlyReachServersWithResources(t *testing.T) {
	with, without, hidden := resourceFixture(t, true, nil), resourceFixture(t, false, nil), resourceFixture(t, true, nil)
	m := resourceManager(t, map[string]string{
		"docs":   fmt.Sprintf(`{"url":%q,"exposure":"direct"}`, with.URL),
		"plain":  fmt.Sprintf(`{"url":%q}`, without.URL),
		"secret": fmt.Sprintf(`{"url":%q,"exposure":"hidden"}`, hidden.URL),
	})
	tool := core.NewRegistry(m.Tools()...)[readResourceTool]
	if tool.(interface{ Deferred() bool }).Deferred() {
		t.Fatal("resource tools not direct although a direct server offers resources")
	}
	if !strings.Contains(tool.Description(), "docs") || strings.Contains(tool.Description(), "secret") {
		t.Fatalf("description=%s", tool.Description())
	}
	for _, name := range []string{"plain", "secret", "absent"} {
		if _, err := callTool(t, m, readResourceTool, `{"server":"`+name+`","uri":"file:///readme.md"}`); err == nil || !strings.Contains(err.Error(), "does not offer resources") {
			t.Fatalf("%s reachable: %v", name, err)
		}
	}
	all, err := callTool(t, m, listResourcesTool, `{}`)
	if err != nil || strings.Contains(textOf(all), `"server": "secret"`) {
		t.Fatalf("hidden server listed: %s %v", textOf(all), err)
	}

	only := resourceManager(t, map[string]string{"plain": fmt.Sprintf(`{"url":%q}`, without.URL)})
	if _, ok := core.NewRegistry(only.Tools()...)[listResourcesTool]; ok {
		t.Fatal("resource tools registered without any server offering resources")
	}
}

func TestResourceListingReportsFailingServersWithoutHidingOthers(t *testing.T) {
	var failing atomic.Bool
	good, bad := resourceFixture(t, true, nil), resourceFixture(t, true, &failing)
	m := resourceManager(t, map[string]string{
		"good": fmt.Sprintf(`{"url":%q}`, good.URL),
		"bad":  fmt.Sprintf(`{"url":%q}`, bad.URL),
	})
	failing.Store(true)
	result, err := callTool(t, m, listResourcesTool, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	text := textOf(result)
	if !strings.Contains(text, `"server": "good"`) || !strings.Contains(text, "resources unavailable") {
		t.Fatalf("listing=%s", text)
	}
}

func TestResourceToolsFollowReconnects(t *testing.T) {
	srv := resourceFixture(t, true, nil)
	m := resourceManager(t, map[string]string{"docs": fmt.Sprintf(`{"url":%q}`, srv.URL)})
	stale := core.NewRegistry(m.Tools()...)[readResourceTool]
	if err := m.Reconnect(context.Background(), "docs"); err != nil {
		t.Fatal(err)
	}
	if result, err := stale.Execute(context.Background(), json.RawMessage(`{"server":"docs","uri":"file:///readme.md"}`), nil); err != nil || textOf(result) != "# Hello" {
		t.Fatalf("resource tool after reconnect: %+v %v", result, err)
	}
	m.Close()
	if _, err := stale.Execute(context.Background(), json.RawMessage(`{"server":"docs","uri":"file:///readme.md"}`), nil); err == nil {
		t.Fatal("resource tool worked after close")
	}
}

func TestResourceToolNamesCannotCollideWithServerTools(t *testing.T) {
	for _, server := range []string{"list", "read", "list_resources", "search"} {
		for _, tool := range []string{"resources", "resource", "resource_templates", "tools", ""} {
			name := toolName(server, tool)
			for _, reserved := range []string{listResourcesTool, listTemplatesTool, readResourceTool, "mcp__search_tools"} {
				if name == reserved {
					t.Fatalf("server %q tool %q takes reserved name %s", server, tool, reserved)
				}
			}
		}
	}
}

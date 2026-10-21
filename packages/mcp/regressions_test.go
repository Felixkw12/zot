package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReconnectRefusesWithdrawnTool(t *testing.T) {
	for _, mode := range []string{"dropped", "expired"} {
		t.Run(mode, func(t *testing.T) {
			var sessions, calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				var req rpcRequest
				if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.ID) == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				result := fixtureResult(req)
				switch req.Method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", fmt.Sprintf("s%d", sessions.Add(1)))
				case "tools/list":
					if sessions.Load() > 1 {
						result = map[string]any{"tools": []any{}}
					}
				case "tools/call":
					if mode == "expired" && sessions.Load() == 1 {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					calls.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			t.Cleanup(srv.Close)
			m, errs := loadOne(t, srv.URL)
			if len(errs) != 0 {
				t.Fatal(errs)
			}
			tool := registry(m)["mcp__s__repo_lookup"]
			if mode == "dropped" {
				m.servers["s"].lost.Store(true)
			}
			_, err := tool.Execute(context.Background(), json.RawMessage(`{}`), nil)
			if err == nil || !strings.Contains(err.Error(), "no longer available") {
				t.Fatalf("withdrawn tool error: %v", err)
			}
			if calls.Load() != 0 || sessions.Load() != 2 {
				t.Fatalf("calls=%d sessions=%d", calls.Load(), sessions.Load())
			}
		})
	}
}

func TestResourceOnlyServerSkipsToolDiscovery(t *testing.T) {
	var toolLists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req rpcRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "tools/list" {
			toolLists.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "unsupported"}})
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"resources": map[string]any{}}, "serverInfo": map[string]any{"name": "resources", "version": "1"}}
		case "resources/list":
			result = map[string]any{"resources": []any{map[string]any{"uri": "file:///readme", "name": "readme"}}}
		case "resources/read":
			result = map[string]any{"contents": []any{map[string]any{"uri": "file:///readme", "text": "hello"}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	m, errs := loadOne(t, srv.URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	m.servers["s"].listed.Store(0)
	m.RefreshStale(context.Background())
	m.relist(m.servers["s"])
	if toolLists.Load() != 0 {
		t.Fatalf("resource-only server received %d tool lists", toolLists.Load())
	}
	listed, err := callTool(t, m, listResourcesTool, `{"server":"s"}`)
	if err != nil || !strings.Contains(textOf(listed), "file:///readme") {
		t.Fatalf("list=%+v err=%v", listed, err)
	}
	read, err := callTool(t, m, readResourceTool, `{"server":"s","uri":"file:///readme"}`)
	if err != nil || textOf(read) != "hello" {
		t.Fatalf("read=%+v err=%v", read, err)
	}
}

func TestSignInUsesAdvertisedMetadata(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			fixture := newOAuthFixture(t)
			var base string
			var metadataHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/mcp":
					if r.Header.Get("Authorization") == "Bearer "+fixture.token {
						var req rpcRequest
						if r.Method != http.MethodPost {
							w.WriteHeader(http.StatusMethodNotAllowed)
							return
						}
						if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.ID) == 0 {
							w.WriteHeader(http.StatusAccepted)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": fixtureResult(req)})
						return
					}
					w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/custom-prm"`)
					w.WriteHeader(http.StatusUnauthorized)
				case "/custom-prm":
					metadataHits.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"resource": base + "/mcp", "authorization_servers": []string{fixture.srv.URL}})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			base = srv.URL
			home := t.TempDir()
			extra := ""
			if explicit {
				extra = `,"oauth":{}`
			}
			writeConfig(t, UserConfigPath(home), fmt.Sprintf(`{"mcpServers":{"s":{"url":%q%s}}}`, base+"/mcp", extra))
			m := New(home, t.TempDir())
			t.Cleanup(m.Close)
			if errs := m.Reload(context.Background()); len(errs) != 1 || m.Snapshot().Servers[0].State != StateSignIn {
				t.Fatalf("errs=%v snapshot=%+v", errs, m.Snapshot())
			}
			previous := openBrowser
			t.Cleanup(func() { openBrowser = previous })
			openBrowser = func(u string) {
				go func() {
					resp, err := http.Get(u)
					if err == nil {
						resp.Body.Close()
					}
				}()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := m.SignIn(ctx, "s", nil); err != nil {
				t.Fatal(err)
			}
			if metadataHits.Load() == 0 {
				t.Fatal("advertised metadata not requested")
			}
			if err := m.Reconnect(ctx, "s"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSignInRejectsCrossOriginMetadata(t *testing.T) {
	var foreignRequests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		http.Error(w, "must not be fetched", http.StatusForbidden)
	}))
	t.Cleanup(foreign.Close)
	fixture := newOAuthFixture(t)
	m, errs := loadOne(t, fixture.srv.URL+"/mcp")
	if len(errs) != 1 {
		t.Fatal(errs)
	}
	// Simulate a compromised server advertising metadata on another origin.
	m.servers["s"].authMetadataURL = foreign.URL + "/metadata"
	previous := openBrowser
	t.Cleanup(func() { openBrowser = previous })
	openBrowser = func(u string) {
		go func() {
			resp, err := http.Get(u)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.SignIn(ctx, "s", nil); err != nil {
		t.Fatal(err)
	}
	if foreignRequests.Load() != 0 {
		t.Fatal("unrelated metadata origin was contacted")
	}
}

func TestProjectEditsOverrideInheritedDefaults(t *testing.T) {
	for _, userEnabled := range []bool{false, true} {
		t.Run(fmt.Sprint(userEnabled), func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			writeConfig(t, UserConfigPath(home), fmt.Sprintf(`{"mcpServers":{"s":{"command":"unused","enabled":%t,"exposure":"direct"}}}`, userEnabled))
			path := ProjectConfigPath(project)
			writeConfig(t, path, `{"mcpServers":{"s":{"enabled":false,"exposure":"hidden","custom":42}}}`)
			if err := SetServerEnabled(path, "s", true); err != nil {
				t.Fatal(err)
			}
			if err := SetServerExposure(path, "s", "deferred"); err != nil {
				t.Fatal(err)
			}
			configs, errs := loadConfig(home, project)
			if len(errs) != 0 {
				t.Fatal(errs)
			}
			cfg := configs["s"]
			if cfg.Enabled == nil || !*cfg.Enabled || cfg.Exposure != "deferred" || cfg.Command != "unused" {
				t.Fatalf("effective config=%+v", cfg)
			}
			data, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(data), `"custom": 42`) {
				t.Fatalf("unknown field lost: %s err=%v", data, err)
			}
		})
	}
}

func TestNullConfigurationEditsReturnErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	writeConfig(t, path, `null`)
	if err := AddServer(path, "s", ServerConfig{Command: "unused"}); err == nil {
		t.Fatal("null root accepted")
	}
	writeConfig(t, path, `{"mcpServers":{"s":null}}`)
	if err := SetServerField(path, "s", "enabled", true); err == nil {
		t.Fatal("null server accepted")
	}
}

func TestNullResourceItemsReturnErrors(t *testing.T) {
	for _, key := range []string{"resources", "resourceTemplates"} {
		t.Run(key, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				var req rpcRequest
				if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.ID) == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				result := fixtureResult(req)
				if req.Method == "initialize" {
					result.(map[string]any)["capabilities"].(map[string]any)["resources"] = map[string]any{}
				} else if strings.HasPrefix(req.Method, "resources/") {
					result = map[string]any{key: []any{nil}}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			t.Cleanup(srv.Close)
			m, errs := loadOne(t, srv.URL)
			if len(errs) != 0 {
				t.Fatal(errs)
			}
			name := listResourcesTool
			if key == "resourceTemplates" {
				name = listTemplatesTool
			}
			_, err := callTool(t, m, name, `{"server":"s"}`)
			if err == nil || !strings.Contains(err.Error(), "invalid") {
				t.Fatalf("null item error=%v", err)
			}
		})
	}
}

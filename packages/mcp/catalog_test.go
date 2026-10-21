package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolNamesAreStableAndProviderCompatible(t *testing.T) {
	if got := toolName("fs-server", "read.file"); got != "mcp__fs_server__read_file" {
		t.Fatalf("name=%s", got)
	}
	prefix := strings.Repeat("x", 100)
	a, b := toolName("server", prefix+"a"), toolName("server", prefix+"b")
	if len(a) != 64 || len(b) != 64 || a == b || a != toolName("server", prefix+"a") {
		t.Fatalf("invalid names: %q %q", a, b)
	}
}

func TestCollidingToolsFailClosed(t *testing.T) {
	for _, crossServer := range []bool{false, true} {
		t.Run(map[bool]string{false: "within-server", true: "across-servers"}[crossServer], func(t *testing.T) {
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
					names := []string{"my-tool", "my_tool"}
					if crossServer {
						names = []string{"c"}
						if r.URL.Path == "/a" {
							names = []string{"b__c"}
						}
					}
					var tools []any
					for _, name := range names {
						tools = append(tools, map[string]any{"name": name, "inputSchema": map[string]string{"type": "object"}})
					}
					result = map[string]any{"tools": tools}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			defer service.Close()
			entries := map[string]any{"srv": map[string]string{"url": service.URL}}
			if crossServer {
				entries = map[string]any{"a": map[string]string{"url": service.URL + "/a"}, "a__b": map[string]string{"url": service.URL + "/b"}}
			}
			data, _ := json.Marshal(map[string]any{"mcpServers": entries})
			home := t.TempDir()
			writeConfig(t, filepath.Join(home, "mcp.json"), string(data))
			m := New(home, t.TempDir())
			defer m.Close()
			errs := m.Reload(context.Background())
			if len(errs) != 1 || len(m.Tools()) != 0 {
				t.Fatalf("errors=%v tools=%v", errs, m.Tools())
			}
		})
	}
}

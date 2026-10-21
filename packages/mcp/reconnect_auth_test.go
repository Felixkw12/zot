package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

func TestRelistConnectionIdentity(t *testing.T) {
	for _, action := range []string{"unrelated-reconnect", "replace", "close"} {
		t.Run(action, func(t *testing.T) {
			var lists atomic.Int32
			started := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusOK)
					return
				}
				var req rpcRequest
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(req.ID) == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				result := fixtureResult(req)
				if req.Method == "initialize" {
					result.(map[string]any)["capabilities"].(map[string]any)["tools"] = map[string]any{"listChanged": true}
				}
				if req.Method == "tools/list" {
					name := "initial"
					if lists.Add(1) == 2 {
						name = "latest"
						close(started)
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
					result = map[string]any{"tools": []any{map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}}}}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			t.Cleanup(srv.Close)
			home := t.TempDir()
			writeConfig(t, UserConfigPath(home), fmt.Sprintf(`{"mcpServers":{"s":{"url":%q},"other":{"enabled":false}}}`, srv.URL))
			m := New(home, t.TempDir())
			t.Cleanup(m.Close)
			t.Cleanup(unblock)
			if errs := m.Reload(context.Background()); len(errs) != 0 {
				t.Fatal(errs)
			}
			changed := make(chan struct{}, 1)
			m.OnChange(func() { changed <- struct{}{} })
			s := m.servers["s"]
			done := make(chan struct{})
			go func() {
				m.relist(s)
				close(done)
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("relist did not start")
			}
			switch action {
			case "unrelated-reconnect":
				if err := m.Reconnect(context.Background(), "other"); err != nil {
					t.Fatal(err)
				}
			case "replace":
				if err := m.Reconnect(context.Background(), "s"); err != nil {
					t.Fatal(err)
				}
			case "close":
				m.Close()
			}
			unblock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("relist did not finish")
			}
			reg := registry(m)
			if action == "unrelated-reconnect" {
				if reg["mcp__s__latest"] == nil || reg["mcp__s__initial"] != nil {
					t.Fatal("unrelated reconnect discarded the tool-list update")
				}
				select {
				case <-changed:
				default:
					t.Fatal("catalog update did not notify the host")
				}
			} else if reg["mcp__s__latest"] != nil {
				t.Fatal("stale discovery restored tools after replacement or shutdown")
			}
		})
	}
}

func TestRuntimeAuthorizationFailureUpdatesState(t *testing.T) {
	for _, method := range []string{"tools/call", "tools/list", "tools/list-poll", "resources/list", "resources/read"} {
		t.Run(method, func(t *testing.T) {
			var revoked atomic.Bool
			var calls atomic.Int32
			const token = "synthetic-access-token"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				var req rpcRequest
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if req.Method == strings.TrimSuffix(method, "-poll") {
					calls.Add(1)
				}
				if revoked.Load() {
					w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://`+r.Host+`/custom-prm"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if r.Header.Get("Authorization") != "Bearer "+token {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if len(req.ID) == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				result := fixtureResult(req)
				if req.Method == "initialize" {
					result.(map[string]any)["capabilities"].(map[string]any)["resources"] = map[string]any{}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			t.Cleanup(srv.Close)
			home := t.TempDir()
			writeConfig(t, UserConfigPath(home), fmt.Sprintf(`{"mcpServers":{"secure":{"url":%q}}}`, srv.URL))
			m := New(home, t.TempDir())
			t.Cleanup(m.Close)
			if err := m.tokens.update(credentialKey("secure", srv.URL), func(entry *storedCredentials, _ bool) {
				*entry = storedCredentials{URL: srv.URL, ClientID: "registered-client", Token: &transport.Token{AccessToken: token, TokenType: "bearer", ExpiresAt: time.Now().Add(time.Hour)}}
			}); err != nil {
				t.Fatal(err)
			}
			if errs := m.Reload(context.Background()); len(errs) != 0 {
				t.Fatal(errs)
			}
			changed := 0
			m.OnChange(func() { changed++ })
			revoked.Store(true)
			stale := registry(m)["mcp__secure__issues"]
			if strings.HasPrefix(method, "tools/list") {
				if !m.relistContext(context.Background(), m.servers["secure"], method != "tools/list-poll") {
					t.Fatal("authorization failure did not change the catalog")
				}
			} else {
				name, args := "mcp__secure__issues", `{}`
				if strings.HasPrefix(method, "resources/") {
					name, args = readResourceTool, `{"server":"secure","uri":"file:///readme"}`
					if method == "resources/list" {
						name, args = listResourcesTool, `{"server":"secure"}`
					}
				}
				_, err := callTool(t, m, name, args)
				if err == nil || !strings.Contains(err.Error(), "needs sign-in") {
					t.Fatalf("expected sign-in error, got %v", err)
				}
			}
			info := m.Snapshot().Servers[0]
			if info.State != StateSignIn || !info.OAuth || !info.SignedIn {
				t.Fatalf("runtime authorization failure did not expose sign-in: state=%q oauth=%v signedIn=%v", info.State, info.OAuth, info.SignedIn)
			}
			wantNotifications := 1
			if method == "tools/list-poll" {
				wantNotifications = 0
			}
			if changed != wantNotifications || len(m.Tools()) != 0 {
				t.Fatalf("failed server capabilities not withdrawn: notifications=%d tools=%d", changed, len(m.Tools()))
			}
			if m.servers["secure"].authMetadataURL != srv.URL+"/custom-prm" {
				t.Fatal("runtime challenge metadata was not retained")
			}
			before := calls.Load()
			if _, err := stale.Execute(context.Background(), json.RawMessage(`{}`), nil); err == nil || calls.Load() != before {
				t.Fatal("stale tool reached a server awaiting sign-in")
			}
			revoked.Store(false)
			if err := m.Reconnect(context.Background(), "secure"); err != nil {
				t.Fatal(err)
			}
			if m.Snapshot().Servers[0].State != StateConnected || registry(m)["mcp__secure__issues"] == nil {
				t.Fatal("restored authorization did not recover the server")
			}
		})
	}
}

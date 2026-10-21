package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

func TestCustomOAuthMetadataSurvivesRefreshAndRestart(t *testing.T) {
	for _, configuredClient := range []bool{false, true} {
		t.Run(fmt.Sprint(configuredClient), func(t *testing.T) {
			fixture := newOAuthFixture(t)
			original := fixture.srv.Config.Handler
			var refreshes atomic.Int32
			fixture.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_ = r.ParseForm()
					if r.Form.Get("grant_type") == "refresh_token" {
						refreshes.Add(1)
						_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fixture.token, "token_type": "bearer", "expires_in": 3600})
						return
					}
				}
				original.ServeHTTP(w, r)
			})
			var base string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/custom-prm":
					_ = json.NewEncoder(w).Encode(map[string]any{"resource": base + "/mcp", "authorization_servers": []string{fixture.srv.URL}})
				case "/mcp":
					if r.Header.Get("Authorization") != "Bearer "+fixture.token {
						w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/custom-prm"`)
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
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
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": fixtureResult(req)})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			base = srv.URL
			home := t.TempDir()
			extra := ""
			if configuredClient {
				extra = `,"oauth":{"clientId":"registered-client"}`
			}
			writeConfig(t, UserConfigPath(home), fmt.Sprintf(`{"mcpServers":{"s":{"url":%q%s}}}`, base+"/mcp", extra))
			m := New(home, t.TempDir())
			t.Cleanup(m.Close)
			if errs := m.Reload(context.Background()); len(errs) != 1 {
				t.Fatal(errs)
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
			if err := m.Reconnect(ctx, "s"); err != nil {
				t.Fatal(err)
			}
			key := credentialKey("s", base+"/mcp")
			for _, restart := range []bool{false, true} {
				if err := m.tokens.update(key, func(entry *storedCredentials, _ bool) {
					entry.Token = &transport.Token{AccessToken: "expired", TokenType: "bearer", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(-time.Hour)}
				}); err != nil {
					t.Fatal(err)
				}
				if restart {
					m.Close()
					m = New(home, t.TempDir())
					t.Cleanup(m.Close)
					if errs := m.Reload(ctx); len(errs) != 0 {
						t.Fatalf("refresh after restart: %v", errs)
					}
				} else if err := m.Reconnect(ctx, "s"); err != nil {
					t.Fatalf("refresh after reconnect: %v", err)
				}
			}
			if refreshes.Load() != 2 {
				t.Fatalf("refreshes=%d", refreshes.Load())
			}
		})
	}
}

func TestStoredOAuthMetadataOriginIsChecked(t *testing.T) {
	const resource = "https://mcp.example.test/mcp"
	m := New(t.TempDir(), t.TempDir())
	for _, metadata := range []string{
		"https://mcp.example.test/custom-prm",
		"https://other.example.test/custom-prm",
		"http://mcp.example.test/custom-prm",
		"https://mcp.example.test:8443/custom-prm",
		"//mcp.example.test/custom-prm",
		"/custom-prm",
		"%",
	} {
		t.Run(metadata, func(t *testing.T) {
			if err := m.tokens.update(credentialKey("s", resource), func(entry *storedCredentials, _ bool) {
				*entry = storedCredentials{URL: resource, ClientID: "stored-client", ResourceMetadataURL: metadata}
			}); err != nil {
				t.Fatal(err)
			}
			cfg := ServerConfig{URL: resource, OAuth: &OAuthConfig{ClientID: "configured-client"}}
			options, err := m.oauthTransportConfig("s", cfg, "")
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if metadata == "https://mcp.example.test/custom-prm" {
				want = metadata
			}
			if options.ProtectedResourceMetadataURL != want || options.ClientID != "configured-client" {
				t.Fatal("stored metadata bypassed the origin check or replaced the configured client")
			}
		})
	}
}

func TestResourceHTTPSessionRecovery(t *testing.T) {
	for _, tool := range []string{listResourcesTool, listTemplatesTool, readResourceTool} {
		for _, withdrawn := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/withdrawn=%v", tool, withdrawn), func(t *testing.T) {
				var sessions, calls atomic.Int32
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
					if req.Method == "initialize" {
						w.Header().Set("Mcp-Session-Id", fmt.Sprintf("s%d", sessions.Add(1)))
					} else if strings.HasPrefix(req.Method, "resources/") {
						switch r.Header.Get("Mcp-Session-Id") {
						case "":
							w.WriteHeader(http.StatusBadRequest)
							return
						case "s1":
							w.WriteHeader(http.StatusNotFound)
							return
						}
						calls.Add(1)
					}
					if len(req.ID) == 0 {
						w.WriteHeader(http.StatusAccepted)
						return
					}
					result := map[string]any{"resources": []any{}, "resourceTemplates": []any{}, "contents": []any{map[string]any{"text": "recovered"}}}
					if req.Method == "resources/list" {
						switch req.Params.Cursor {
						case "":
							result["nextCursor"] = "2"
						case "2":
							result["nextCursor"] = "3"
						}
					}
					if req.Method == "initialize" {
						capabilities := map[string]any{}
						if !withdrawn || sessions.Load() == 1 {
							capabilities["resources"] = map[string]any{}
						}
						result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": capabilities, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
				}))
				t.Cleanup(srv.Close)
				m, errs := loadOne(t, srv.URL)
				if len(errs) != 0 {
					t.Fatal(errs)
				}
				args := `{"server":"s","uri":"file:///readme"}`
				wantCalls := int32(1)
				if tool == listResourcesTool && !withdrawn {
					// Listing all resources must use the replacement connection
					// on later pages, not the one held before the first retry.
					args, wantCalls = `{}`, 3
				}
				result, err := callTool(t, m, tool, args)
				if withdrawn {
					if err == nil || calls.Load() != 0 {
						t.Fatalf("withdrawn resources called: err=%v calls=%d", err, calls.Load())
					}
				} else if err != nil || calls.Load() != wantCalls || strings.Contains(textOf(result), `"errors"`) {
					t.Fatalf("resource request did not recover: err=%v calls=%d result=%s", err, calls.Load(), textOf(result))
				}
				if sessions.Load() != 2 {
					t.Fatalf("sessions=%d", sessions.Load())
				}
			})
		}
	}
}

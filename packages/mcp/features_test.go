package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/patriceckhart/zot/packages/core"
)

func TestToolExposurePatterns(t *testing.T) {
	cfg := ServerConfig{Exposure: "hidden", ToolExposure: map[string]string{"repo.*": "direct", "repo.lookup": "deferred", "*": "hidden", "is*": "direct"}}
	for tool, want := range map[string]string{"repo.lookup": "deferred", "repo.list": "direct", "issues": "direct", "other": "hidden"} {
		if got := cfg.exposureFor(tool); got != want {
			t.Fatalf("%s=%s want %s", tool, got, want)
		}
	}
	for pattern, name := range map[string]string{"a*c": "abc", "*": "", "a*b*c": "aXbYc", "*end": "the end"} {
		if !matchPattern(pattern, name) {
			t.Fatalf("%q should match %q", pattern, name)
		}
	}
	for pattern, name := range map[string]string{"a*c": "abd", "a*b*c": "acb", "x*": "yx"} {
		if matchPattern(pattern, name) {
			t.Fatalf("%q should not match %q", pattern, name)
		}
	}
}

func TestToolExposureShapesCatalog(t *testing.T) {
	m, _ := httpManager(t, `,"exposure":"hidden","toolExposure":{"repo.*":"direct"}`)
	reg := registry(m)
	if _, ok := reg["mcp__docs__issues"]; ok {
		t.Fatal("hidden tool registered")
	}
	tool, ok := reg["mcp__docs__repo_lookup"]
	if !ok {
		t.Fatalf("selected tool missing: %v", reg)
	}
	if tool.(interface{ Deferred() bool }).Deferred() {
		t.Fatal("direct tool deferred")
	}
	info := m.Snapshot().Servers[0]
	if !info.ToolExposure || info.Tools[0].Exposure == info.Tools[1].Exposure {
		t.Fatalf("per-tool exposure not reported: %+v", info)
	}
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), `{"mcpServers":{"s":{"command":"x","toolExposure":{"a":"loud"}}}}`)
	if _, errs := loadConfig(home, t.TempDir()); len(errs) != 1 || !strings.Contains(errs[0].Error(), "toolExposure") {
		t.Fatalf("errors=%v", errs)
	}
}

func TestProjectOverrideKeepsUserConnection(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("ZOT_MCP_OVERRIDE_TOKEN", "synthetic")
	writeConfig(t, filepath.Join(home, "mcp.json"), `{"mcpServers":{
 "docs":{"url":"https://example.test/mcp","headers":{"Authorization":"Bearer ${ZOT_MCP_OVERRIDE_TOKEN}"},"exposure":"direct"},
 "tools":{"command":"user-tool"}}}`)
	writeConfig(t, filepath.Join(project, ".zot", "mcp.json"), `{"mcpServers":{
 "docs":{"exposure":"hidden","toolExposure":{"search":"direct"}},
 "tools":{"command":"project-tool"}}}`)
	cfg, errs := loadConfig(home, project)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	docs := cfg["docs"]
	if docs.URL != "https://example.test/mcp" || docs.Headers["Authorization"] != "Bearer synthetic" || docs.Exposure != "hidden" || docs.ToolExposure["search"] != "direct" {
		t.Fatalf("docs=%+v", docs)
	}
	if cfg["tools"].Command != "project-tool" {
		t.Fatalf("full project definition did not replace the user entry: %+v", cfg["tools"])
	}
}

// listChangeServer is a stdio-free streamable HTTP fixture whose tool list
// can change, and which announces changes on its event stream.
type listChangeServer struct {
	mu            sync.Mutex
	tools         []string
	events        chan string
	status        atomic.Int32 // non-zero forces this status on POSTs
	posts         atomic.Int32
	noListChanged bool
}

func (f *listChangeServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			for {
				select {
				case data := <-f.events:
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		f.posts.Add(1)
		if code := f.status.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		var req rpcRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := fixtureResult(req)
		if req.Method == "initialize" && !f.noListChanged {
			result.(map[string]any)["capabilities"].(map[string]any)["tools"] = map[string]any{"listChanged": true}
		}
		if req.Method == "tools/list" {
			f.mu.Lock()
			var tools []any
			for _, name := range f.tools {
				tools = append(tools, map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}})
			}
			f.mu.Unlock()
			result = map[string]any{"tools": tools}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	})
}

func TestToolListChangesUpdateTheCatalog(t *testing.T) {
	fixture := &listChangeServer{tools: []string{"old"}, events: make(chan string, 1)}
	srv := httptest.NewServer(fixture.handler(t))
	t.Cleanup(srv.Close)
	m, errs := loadOne(t, srv.URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	changed := make(chan struct{}, 4)
	m.OnChange(func() { changed <- struct{}{} })
	fixture.mu.Lock()
	fixture.tools = []string{"new"}
	fixture.mu.Unlock()
	fixture.events <- `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("list change was not applied")
	}
	reg := registry(m)
	if _, ok := reg["mcp__s__new"]; !ok {
		t.Fatalf("new tool missing: %v", reg)
	}
	if _, ok := reg["mcp__s__old"]; ok {
		t.Fatal("withdrawn tool still callable")
	}
}

func TestHTTPRetriesTransientFailuresButNotToolCalls(t *testing.T) {
	var lists, calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch req.Method {
		case "tools/list":
			if lists.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		case "tools/call":
			calls.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": fixtureResult(req)})
	}))
	t.Cleanup(srv.Close)
	m, errs := loadOne(t, srv.URL)
	if len(errs) != 0 {
		t.Fatalf("transient discovery failure not retried: %v", errs)
	}
	if _, err := core.NewRegistry(m.Tools()...)["mcp__s__issues"].Execute(context.Background(), json.RawMessage(`{}`), nil); err == nil {
		t.Fatal("failing call succeeded")
	}
	if calls.Load() != 1 {
		t.Fatalf("tool call sent %d times, must not be retried", calls.Load())
	}
}

func TestReconnectRestartsOnlyOneServer(t *testing.T) {
	first, second := &listChangeServer{tools: []string{"a"}, events: make(chan string)}, &listChangeServer{tools: []string{"b"}, events: make(chan string)}
	one, two := httptest.NewServer(first.handler(t)), httptest.NewServer(second.handler(t))
	t.Cleanup(one.Close)
	t.Cleanup(two.Close)
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), fmt.Sprintf(`{"mcpServers":{"one":{"url":%q},"two":{"url":%q}}}`, one.URL, two.URL))
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	if errs := m.Reload(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	m.mu.RLock()
	untouched := m.servers["two"]
	m.mu.RUnlock()
	first.mu.Lock()
	first.tools = []string{"a", "a2"}
	first.mu.Unlock()
	if err := m.Reconnect(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	same := m.servers["two"] == untouched
	m.mu.RUnlock()
	if !same {
		t.Fatal("reconnecting one server restarted another")
	}
	if _, ok := registry(m)["mcp__one__a2"]; !ok {
		t.Fatal("reconnect did not refresh the tool list")
	}
	if err := m.Reconnect(context.Background(), "absent"); err == nil {
		t.Fatal("unknown server reconnected")
	}
}

// oauthFixture is a local MCP server protected by a local authorization
// server with dynamic client registration and PKCE.
type oauthFixture struct {
	srv       *httptest.Server
	token     string
	verifiers sync.Map
	clients   atomic.Int32
}

func newOAuthFixture(t *testing.T) *oauthFixture {
	f := &oauthFixture{token: "synthetic-access-token"}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	base := f.srv.URL
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
			"registration_endpoint": base + "/register", "response_types_supported": []string{"code"},
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		f.clients.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "registered-client"})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge") == "" || q.Get("client_id") != "registered-client" {
			http.Error(w, "bad request", 400)
			return
		}
		f.verifiers.Store("code-1", q.Get("code_challenge"))
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=code-1&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "code-1" || r.Form.Get("code_verifier") == "" {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.token, "token_type": "bearer", "expires_in": 3600})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource/mcp"`)
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
	})
	return f
}

func TestOAuthSignInStoresTokenAndConnects(t *testing.T) {
	fixture := newOAuthFixture(t)
	previous := openBrowser
	t.Cleanup(func() { openBrowser = previous })
	// The fake browser follows the authorization redirect to the callback.
	openBrowser = func(u string) {
		go func() {
			resp, err := http.Get(u)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), fmt.Sprintf(`{"mcpServers":{"secure":{"url":%q}}}`, fixture.srv.URL+"/mcp"))
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	errs := m.Reload(context.Background())
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "sign-in required") {
		t.Fatalf("errors=%v", errs)
	}
	info := m.Snapshot().Servers[0]
	if info.State != StateSignIn || !info.OAuth || info.SignedIn {
		t.Fatalf("before sign-in: %+v", info)
	}
	var shown string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.SignIn(ctx, "secure", func(s SignInStatus) { shown = s.URL }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shown, "/authorize") {
		t.Fatalf("authorization URL not reported: %q", shown)
	}
	auth := filepath.Join(home, "mcp-auth.json")
	if runtime.GOOS != "windows" {
		if stat, err := os.Stat(auth); err != nil || stat.Mode().Perm() != 0o600 {
			t.Fatalf("token file: %v %v", stat, err)
		}
	}
	if err := m.Reconnect(context.Background(), "secure"); err != nil {
		t.Fatal(err)
	}
	info = m.Snapshot().Servers[0]
	if info.State != StateConnected || !info.SignedIn || info.ToolCount != 2 {
		t.Fatalf("after sign-in: %+v", info)
	}
	if _, err := registry(m)["mcp__secure__issues"].Execute(context.Background(), json.RawMessage(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	if fixture.clients.Load() != 1 {
		t.Fatalf("registrations=%d", fixture.clients.Load())
	}
	if err := m.SignOut("secure"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(auth); strings.Contains(string(data), fixture.token) {
		t.Fatal("sign-out kept the token")
	}
	if err := m.Reconnect(context.Background(), "secure"); err == nil || m.Snapshot().Servers[0].State != StateSignIn {
		t.Fatalf("signed-out server connected: %v", err)
	}
}

func TestStaticAuthorizationHeaderDisablesOAuth(t *testing.T) {
	cfg := ServerConfig{Transport: "streamable-http", Headers: map[string]string{"authorization": "Bearer x"}}
	if oauthEnabled(cfg) {
		t.Fatal("OAuth used despite a static header")
	}
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), `{"mcpServers":{"s":{"command":"x","oauth":{}}}}`)
	if _, errs := loadConfig(home, t.TempDir()); len(errs) != 1 {
		t.Fatalf("oauth accepted for stdio: %v", errs)
	}
}

func TestExpiredOAuthTokenIsRefreshed(t *testing.T) {
	fixture := newOAuthFixture(t)
	var refreshes atomic.Int32
	mux := fixture.srv.Config.Handler.(*http.ServeMux)
	// Wrap the token endpoint: a refresh_token grant issues a new token.
	fixture.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			if r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == "synthetic-refresh" {
				refreshes.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fixture.token, "token_type": "bearer", "expires_in": 3600})
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
	home := t.TempDir()
	url := fixture.srv.URL + "/mcp"
	writeConfig(t, filepath.Join(home, "mcp.json"), fmt.Sprintf(`{"mcpServers":{"secure":{"url":%q}}}`, url))
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	expired := storedCredentials{URL: url, ClientID: "registered-client"}
	expired.Token = &transport.Token{AccessToken: "stale", TokenType: "bearer", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(-time.Hour)}
	if err := m.tokens.update(credentialKey("secure", url), func(entry *storedCredentials, _ bool) { *entry = expired }); err != nil {
		t.Fatal(err)
	}
	if errs := m.Reload(context.Background()); len(errs) != 0 {
		t.Fatalf("expired token not refreshed: %v", errs)
	}
	if refreshes.Load() == 0 {
		t.Fatal("refresh grant not used")
	}
	entry, _, _ := m.tokens.load(credentialKey("secure", url))
	if entry.Token == nil || entry.Token.AccessToken != fixture.token {
		t.Fatalf("refreshed token not stored: %+v", entry.Token)
	}
}

func TestEndedHTTPSessionIsRestartedAndCallSentOnce(t *testing.T) {
	var sessions, calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req rpcRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(400)
			return
		}
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("s%d", sessions.Add(1)))
		} else if r.Header.Get("Mcp-Session-Id") == "s1" && req.Method == "tools/call" {
			// The first session has expired on the server side.
			w.WriteHeader(http.StatusNotFound)
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
	t.Cleanup(srv.Close)
	m, errs := loadOne(t, srv.URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	result, err := core.NewRegistry(m.Tools()...)["mcp__s__repo_lookup"].Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if sessions.Load() != 2 || calls.Load() != 1 {
		t.Fatalf("sessions=%d calls=%d", sessions.Load(), calls.Load())
	}
}

func TestSignInIgnoresForeignCallbacksAndReusesTheClient(t *testing.T) {
	fixture := newOAuthFixture(t)
	previous := openBrowser
	t.Cleanup(func() { openBrowser = previous })
	follow := func(u string) {
		resp, err := http.Get(u)
		if err == nil {
			resp.Body.Close()
		}
	}
	// A request with a foreign state arrives first. It must be refused
	// without ending the sign-in, then the real redirect completes it.
	forged := make(chan int, 1)
	openBrowser = func(u string) {
		go func() {
			parsed, _ := url.Parse(u)
			resp, err := http.Get(parsed.Query().Get("redirect_uri") + "?code=code-1&state=forged")
			if err == nil {
				forged <- resp.StatusCode
				resp.Body.Close()
			}
			follow(u)
		}()
	}
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), fmt.Sprintf(`{"mcpServers":{"secure":{"url":%q}}}`, fixture.srv.URL+"/mcp"))
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	m.Reload(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.SignIn(ctx, "secure", nil); err != nil {
		t.Fatalf("forged callback ended the sign-in: %v", err)
	}
	if code := <-forged; code != http.StatusBadRequest {
		t.Fatalf("forged callback status=%d", code)
	}
	openBrowser = func(u string) { go follow(u) }
	for range 2 {
		if err := m.SignIn(ctx, "secure", nil); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.clients.Load() != 1 {
		t.Fatalf("each sign-in registered a new client: %d", fixture.clients.Load())
	}
}

func TestStaleToolUsesTheCurrentConnectionOrFails(t *testing.T) {
	fixture := &listChangeServer{tools: []string{"a", "b"}, events: make(chan string)}
	srv := httptest.NewServer(fixture.handler(t))
	t.Cleanup(srv.Close)
	m, errs := loadOne(t, srv.URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	stale := registry(m)
	fixture.mu.Lock()
	fixture.tools = []string{"a"}
	fixture.mu.Unlock()
	if err := m.Reconnect(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	// A tool held from before the reconnect reaches the new connection.
	if _, err := stale["mcp__s__a"].Execute(context.Background(), json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("still offered tool failed: %v", err)
	}
	// A withdrawn tool is refused without a request to the server.
	before := fixture.posts.Load()
	if _, err := stale["mcp__s__b"].Execute(context.Background(), json.RawMessage(`{}`), nil); err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("withdrawn tool: %v", err)
	}
	if fixture.posts.Load() != before {
		t.Fatal("withdrawn tool reached the server")
	}
	m.Close()
	if _, err := stale["mcp__s__a"].Execute(context.Background(), json.RawMessage(`{}`), nil); err == nil || !strings.Contains(err.Error(), "no longer available") {
		t.Fatalf("after close: %v", err)
	}
}

func TestToolWithdrawnByListChangeIsRefused(t *testing.T) {
	fixture := &listChangeServer{tools: []string{"a", "b"}, events: make(chan string, 1)}
	srv := httptest.NewServer(fixture.handler(t))
	t.Cleanup(srv.Close)
	m, _ := loadOne(t, srv.URL)
	stale := registry(m)
	changed := make(chan struct{}, 1)
	m.OnChange(func() { changed <- struct{}{} })
	fixture.mu.Lock()
	fixture.tools = []string{"a"}
	fixture.mu.Unlock()
	fixture.events <- `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no change")
	}
	before := fixture.posts.Load()
	_, err := stale["mcp__s__b"].Execute(context.Background(), json.RawMessage(`{}`), nil)
	if err == nil || fixture.posts.Load() != before {
		t.Fatalf("withdrawn tool reached the server: %v", err)
	}
	if _, err := stale["mcp__s__a"].Execute(context.Background(), json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("remaining tool failed: %v", err)
	}
}

// noStreamServer serves tools over streamable HTTP without an event
// stream, so it cannot announce tool-list changes.
func noStreamServer(t *testing.T, tools func() []string, lists *atomic.Int32) *httptest.Server {
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
		case "tools/list":
			lists.Add(1)
			var out []any
			for _, name := range tools() {
				out = append(out, map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}})
			}
			result = map[string]any{"tools": out}
		case "tools/call":
			known := false
			for _, name := range tools() {
				known = known || name == req.Params.Name
			}
			if !known {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "unknown tool"}})
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServersWithoutEventStreamAreRelistedWhenStale(t *testing.T) {
	previous := relistInterval.Load()
	t.Cleanup(func() { relistInterval.Store(previous) })
	relistInterval.Store(int64(time.Hour))
	var mu sync.Mutex
	names := []string{"old"}
	var lists atomic.Int32
	srv := noStreamServer(t, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), names...) }, &lists)
	m, errs := loadOne(t, srv.URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	m.mu.RLock()
	pushes := m.servers["s"].pushesChanges()
	m.mu.RUnlock()
	if pushes {
		t.Fatal("server without event stream treated as pushing changes")
	}
	mu.Lock()
	names = []string{"new"}
	mu.Unlock()
	before := lists.Load()
	if m.RefreshStale(context.Background()) || lists.Load() != before {
		t.Fatal("fresh list read again")
	}
	relistInterval.Store(0)
	if !m.RefreshStale(context.Background()) {
		t.Fatal("stale list not refreshed")
	}
	reg := registry(m)
	if _, ok := reg["mcp__s__new"]; !ok {
		t.Fatalf("new tool missing: %v", reg)
	}
	if _, ok := reg["mcp__s__old"]; ok {
		t.Fatal("withdrawn tool kept")
	}
	if m.RefreshStale(context.Background()) {
		t.Fatal("unchanged list reported as changed")
	}
}

func TestServersWithEventStreamAreNotPolled(t *testing.T) {
	previous := relistInterval.Load()
	t.Cleanup(func() { relistInterval.Store(previous) })
	relistInterval.Store(0)
	fixture := &listChangeServer{tools: []string{"a"}, events: make(chan string)}
	srv := httptest.NewServer(fixture.handler(t))
	t.Cleanup(srv.Close)
	m, errs := loadOne(t, srv.URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.RLock()
		pushes := m.servers["s"].pushesChanges()
		m.mu.RUnlock()
		if pushes {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("event stream not detected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := fixture.posts.Load()
	m.RefreshStale(context.Background())
	if fixture.posts.Load() != before {
		t.Fatal("server with event stream was polled")
	}
}

func TestUnknownToolErrorRelistsTheServer(t *testing.T) {
	previous := relistInterval.Load()
	t.Cleanup(func() { relistInterval.Store(previous) })
	relistInterval.Store(int64(time.Hour))
	var mu sync.Mutex
	names := []string{"gone", "kept"}
	var lists atomic.Int32
	srv := noStreamServer(t, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), names...) }, &lists)
	m, errs := loadOne(t, srv.URL)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	changed := make(chan struct{}, 1)
	m.OnChange(func() { changed <- struct{}{} })
	mu.Lock()
	names = []string{"kept"}
	mu.Unlock()
	if _, err := registry(m)["mcp__s__gone"].Execute(context.Background(), json.RawMessage(`{}`), nil); err == nil {
		t.Fatal("withdrawn tool call succeeded")
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("unknown tool did not trigger a relist")
	}
	if _, ok := registry(m)["mcp__s__gone"]; ok {
		t.Fatal("withdrawn tool still in the catalog")
	}
}

func TestParsePastedRedirect(t *testing.T) {
	for input, want := range map[string][3]string{
		"http://127.0.0.1:5555/callback?code=abc&state=xyz": {"abc", "xyz", ""},
		"  abc#xyz  ":                    {"abc", "xyz", ""},
		"abc":                            {"abc", "", ""},
		"":                               {"", "", "paste"},
		"http://127.0.0.1/callback?x=1":  {"", "", "no code"},
		"http://h/callback?error=denied": {"", "", "refused"},
		"two words":                      {"", "", "full address"},
	} {
		code, state, problem := parsePastedRedirect(input)
		if code != want[0] || state != want[1] || (want[2] == "") != (problem == "") || !strings.Contains(problem, want[2]) {
			t.Fatalf("%q: code=%q state=%q problem=%q", input, code, state, problem)
		}
	}
}

// remoteBrowser simulates a browser on another machine: it follows the
// authorization but cannot reach the local callback, so it reports the
// address it was redirected to.
func remoteBrowser(t *testing.T) chan string {
	t.Helper()
	landed := make(chan string, 1)
	previous := openBrowser
	t.Cleanup(func() { openBrowser = previous })
	openBrowser = func(u string) {
		go func() {
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := client.Get(u)
			if err != nil {
				return
			}
			resp.Body.Close()
			landed <- resp.Header.Get("Location")
		}()
	}
	return landed
}

func TestSignInCompletesWithPastedRedirect(t *testing.T) {
	fixture := newOAuthFixture(t)
	landed := remoteBrowser(t)
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), fmt.Sprintf(`{"mcpServers":{"secure":{"url":%q}}}`, fixture.srv.URL+"/mcp"))
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	m.Reload(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	results := make(chan [2]error, 1)
	err := m.SignIn(ctx, "secure", func(status SignInStatus) {
		go func() {
			address := <-landed
			refused := status.Paste(strings.Replace(address, "state=", "state=other", 1))
			results <- [2]error{refused, status.Paste(address)}
		}()
	})
	if err != nil {
		t.Fatalf("pasted redirect did not complete the sign-in: %v", err)
	}
	pasted := <-results
	refused, accepted := pasted[0], pasted[1]
	if refused == nil || !strings.Contains(refused.Error(), "another sign-in") {
		t.Fatalf("foreign state accepted: %v", refused)
	}
	if accepted != nil {
		t.Fatal(accepted)
	}
	if !m.signedIn("secure", m.servers["secure"].cfg) {
		t.Fatal("token not stored")
	}
}

func TestTerminalPasteCompletesTextLogin(t *testing.T) {
	fixture := newOAuthFixture(t)
	landed := remoteBrowser(t)
	reader, writer := io.Pipe()
	previous := openTerminal
	t.Cleanup(func() { openTerminal = previous })
	openTerminal = func() (io.ReadCloser, error) { return reader, nil }
	go func() {
		address := <-landed
		fmt.Fprintln(writer, "not an address")
		fmt.Fprintln(writer, address)
	}()
	home := t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), fmt.Sprintf(`{"mcpServers":{"secure":{"url":%q}}}`, fixture.srv.URL+"/mcp"))
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	m.Reload(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := m.Command(ctx, "login secure")
	if err != nil || !strings.Contains(out, "secure: connected") {
		t.Fatalf("out=%s err=%v", out, err)
	}
}

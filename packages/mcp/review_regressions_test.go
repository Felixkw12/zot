package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

func TestConcurrentOAuthRefreshReusesRotatedToken(t *testing.T) {
	fixture := newOAuthFixture(t)
	original := fixture.srv.Config.Handler
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var refreshes atomic.Int32
	fixture.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			original.ServeHTTP(w, r)
			return
		}
		if refreshes.Add(1) != 1 {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fixture.token, "refresh_token": "rotated-refresh", "token_type": "bearer", "expires_in": 3600})
	})
	home := t.TempDir()
	url := fixture.srv.URL + "/mcp"
	writeConfig(t, filepath.Join(home, "mcp.json"), `{"mcpServers":{"secure":{"url":"`+url+`"}}}`)
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	key := credentialKey("secure", url)
	if err := m.tokens.update(key, func(entry *storedCredentials, _ bool) {
		entry.URL, entry.ClientID = url, "registered-client"
		entry.Token = &transport.Token{AccessToken: fixture.token, TokenType: "bearer", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour)}
	}); err != nil {
		t.Fatal(err)
	}
	if errs := m.Reload(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if err := m.tokens.update(key, func(entry *storedCredentials, _ bool) {
		entry.Token.ExpiresAt = time.Now().Add(-time.Hour)
	}); err != nil {
		t.Fatal(err)
	}
	tool := registry(m)["mcp__secure__issues"]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := tool.Execute(ctx, json.RawMessage(`{}`), nil)
		finished <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("refresh did not start")
	}
	// The other caller must wait without replaying a rotating refresh token,
	// and cancelling that wait must not cancel the first caller's refresh.
	waiting, stopWaiting := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stopWaiting()
	_, err := tool.Execute(waiting, json.RawMessage(`{}`), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		unblock()
		t.Fatalf("waiting call did not honor its deadline: %v", err)
	}
	// A second adapter for the same credential must share the gate too,
	// rather than independently refreshing on a replacement connection.
	options, err := m.oauthTransportConfig("secure", ServerConfig{URL: url}, "")
	if err != nil {
		unblock()
		t.Fatal(err)
	}
	otherWait, stopOtherWait := context.WithTimeout(ctx, 100*time.Millisecond)
	defer stopOtherWait()
	_, err = options.TokenStore.GetToken(otherWait)
	unblock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("another adapter did not share the cancellable refresh gate: %v", err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("refresh did not finish")
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("rotated token was not reused: %v", err)
	}
	if token, err := options.TokenStore.GetToken(ctx); err != nil || token == nil || token.RefreshToken != "rotated-refresh" {
		t.Fatal("another adapter did not reuse the rotated token")
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh token replayed %d times", refreshes.Load())
	}
	entry, _, err := m.tokens.load(key)
	if err != nil || entry.Token == nil || entry.Token.RefreshToken != "rotated-refresh" {
		t.Fatal("rotated refresh token was not persisted")
	}
	if m.Snapshot().Servers[0].State != StateConnected {
		t.Fatal("successful refresh withdrew server capabilities")
	}
}

func TestRejectedOAuthRefreshNeedsSignIn(t *testing.T) {
	fixture := newOAuthFixture(t)
	home := t.TempDir()
	url := fixture.srv.URL + "/mcp"
	writeConfig(t, filepath.Join(home, "mcp.json"), `{"mcpServers":{"secure":{"url":"`+url+`"}}}`)
	m := New(home, t.TempDir())
	t.Cleanup(m.Close)
	if err := m.tokens.update(credentialKey("secure", url), func(entry *storedCredentials, _ bool) {
		entry.URL, entry.ClientID = url, "registered-client"
		entry.Token = &transport.Token{AccessToken: "expired", TokenType: "bearer", RefreshToken: "rejected", ExpiresAt: time.Now().Add(-time.Hour)}
	}); err != nil {
		t.Fatal(err)
	}
	// The fixture rejects refresh grants. A genuinely unusable token must
	// still require sign-in rather than connecting with expired credentials.
	if errs := m.Reload(context.Background()); len(errs) != 1 {
		t.Fatalf("rejected refresh errors: %v", errs)
	}
	if m.Snapshot().Servers[0].State != StateSignIn || len(m.Tools()) != 0 {
		t.Fatal("rejected refresh did not withdraw server capabilities")
	}
}

func TestDiscoveryQueuesToolListNotifications(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "replacement"}[replacement], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var lists atomic.Int32
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
				if req.Method == "tools/list" {
					name := "latest"
					if lists.Add(1) == 1 {
						name = "stale"
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
			m := New(t.TempDir(), t.TempDir())
			t.Cleanup(m.Close)
			s := &server{name: "s", cfg: ServerConfig{URL: srv.URL}}
			if err := s.cfg.validate(t.TempDir()); err != nil {
				t.Fatal(err)
			}
			if replacement {
				m.servers[s.name] = &server{name: s.name}
			}
			finished := make(chan error, 1)
			go func() { finished <- m.start(ctx, s) }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("initial tool discovery did not start")
			}
			// Use the notification handler's relist entry point synchronously
			// so the test does not depend on reader goroutine scheduling.
			m.relist(s)
			m.relist(s)
			s.relistMu.Lock()
			pending := s.relistPending
			s.relistMu.Unlock()
			unblock()
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("initial tool discovery did not finish")
			}
			t.Cleanup(s.shutdown)
			if !pending {
				t.Fatal("notification during initial discovery was discarded")
			}
			changed := make(chan struct{}, 1)
			m.OnChange(func() { changed <- struct{}{} })
			m.mu.Lock()
			m.servers[s.name] = s
			m.rebuildLocked()
			m.mu.Unlock()
			m.activateRelisting(s)
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatal("publication did not drain the pending notification")
			}
			reg := registry(m)
			if reg["mcp__s__latest"] == nil || reg["mcp__s__stale"] != nil || lists.Load() != 2 {
				t.Fatal("discovery notifications did not coalesce into an updated catalog")
			}
		})
	}
}

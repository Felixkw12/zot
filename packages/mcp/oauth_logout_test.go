package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

func TestSignOutInvalidatesPendingRefresh(t *testing.T) {
	m := New(t.TempDir(), t.TempDir())
	cfg := ServerConfig{Transport: "streamable-http", URL: "https://synthetic.invalid/mcp"}
	m.servers["s"] = &server{name: "s", cfg: cfg}
	options, err := m.oauthTransportConfig("s", cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	adapter := options.TokenStore.(*serverTokens)
	expired := &transport.Token{AccessToken: "synthetic-old", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(-time.Hour)}
	if err := adapter.SaveToken(context.Background(), expired); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	adapter.refresh = func(ctx context.Context, _ string) (*transport.Token, error) {
		close(entered)
		<-release
		token := &transport.Token{AccessToken: "synthetic-new", ExpiresAt: time.Now().Add(time.Hour)}
		return token, adapter.SaveToken(ctx, token)
	}
	done := make(chan error, 1)
	go func() {
		_, err := adapter.GetToken(context.Background())
		done <- err
	}()
	<-entered
	err = m.SignOut("s")
	close(release)
	refreshErr := <-done
	if err != nil {
		t.Fatal(err)
	}
	if m.signedIn("s", cfg) {
		t.Fatal("in-flight refresh restored credentials after successful sign-out")
	}
	if !errors.Is(refreshErr, transport.ErrOAuthAuthorizationRequired) {
		t.Fatalf("stale refresh error=%v", refreshErr)
	}
	// A pending authorization-code exchange uses the same adapter. It
	// must not resurrect credentials either, even after a new login.
	fresh, err := m.oauthTransportConfig("s", cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	token := &transport.Token{AccessToken: "synthetic-fresh", ExpiresAt: time.Now().Add(time.Hour)}
	if err := fresh.TokenStore.SaveToken(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if err := adapter.SaveToken(context.Background(), expired); !errors.Is(err, transport.ErrOAuthAuthorizationRequired) {
		t.Fatalf("stale exchange error=%v", err)
	}
	if _, err := adapter.GetToken(context.Background()); !errors.Is(err, transport.ErrOAuthAuthorizationRequired) {
		t.Fatalf("old connection can read new credentials: %v", err)
	}
	got, err := fresh.TokenStore.GetToken(context.Background())
	if err != nil || got.AccessToken != token.AccessToken {
		t.Fatalf("new login was not retained: %v", err)
	}
}

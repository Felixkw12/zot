package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
)

// Sign-in failures keep the structured OAuth error but never quote a raw
// response body or URL from the library's messages.
func TestOAuthErrorsAreDescribedWithoutRawResponses(t *testing.T) {
	cases := map[error]string{
		fmt.Errorf("registration request failed: %w", transport.OAuthError{ErrorCode: "invalid_client_metadata", ErrorDescription: "bad redirect"}): "authorization server answered invalid_client_metadata - bad redirect",
		errors.New("registration request failed with status 500: <html>secret-body https://auth.example.test/register</html>"):                      "refused the client registration",
		errors.New("failed to get server metadata: discovery at \"https://auth.example.test/secret\" returned no metadata"):                         "metadata could not be discovered",
		errors.New("failed to send token request: dial tcp: lookup auth.example.test: no such host"):                                                "could not be reached",
		errors.New("token request failed with status 400: {\"raw\":\"secret-body\"}"):                                                               "rejected the request",
	}
	for input, want := range cases {
		got := describeOAuthError(input)
		if !strings.Contains(got.Error(), want) || strings.Contains(got.Error(), "secret") || strings.Contains(got.Error(), "example.test") {
			t.Fatalf("%v described as %q, want %q", input, got, want)
		}
	}
	if got := describeOAuthError(context.Canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("cancellation rewritten: %v", got)
	}
	if describeOAuthError(nil) != nil {
		t.Fatal("nil rewritten")
	}
}

// Refresh configuration reuses the redirect URI of the stored client
// registration instead of a placeholder port.
func TestRefreshConfigReusesStoredRedirectURI(t *testing.T) {
	m := New(t.TempDir(), t.TempDir())
	cfg := ServerConfig{Transport: "streamable-http", URL: "https://mcp.example.test/mcp"}
	key := credentialKey("s", cfg.URL)
	if err := m.tokens.update(key, func(entry *storedCredentials, _ bool) {
		entry.URL, entry.ClientID, entry.RedirectURI = cfg.URL, "registered", "http://127.0.0.1:43210/callback"
	}); err != nil {
		t.Fatal(err)
	}
	out, err := m.oauthTransportConfig("s", cfg, "")
	if err != nil || out.RedirectURI != "http://127.0.0.1:43210/callback" || out.ClientID != "registered" {
		t.Fatalf("config=%+v err=%v", out, err)
	}
	cfg.OAuth = &OAuthConfig{CallbackPort: 8765}
	if out, _ := m.oauthTransportConfig("s", cfg, ""); out.RedirectURI != redirectURI(8765) {
		t.Fatalf("configured port ignored: %s", out.RedirectURI)
	}
}

func TestAuthorizationFailureOnlyAffectsCurrentOAuthConnection(t *testing.T) {
	cfg := ServerConfig{Transport: "streamable-http", URL: "https://mcp.example.test/mcp"}
	err := &transport.AuthorizationRequiredError{ResourceMetadataURL: "https://other.example.test/prm"}
	m := New(t.TempDir(), t.TempDir())
	old := &server{name: "s", cfg: cfg}
	current := &server{name: "s", cfg: cfg, authMetadataURL: "https://mcp.example.test/prm"}
	m.servers["s"] = current
	if m.markSignInRequired(old, err, true) || current.err != nil {
		t.Fatal("late authorization failure changed the replacement connection")
	}
	if m.markSignInRequired(current, errors.New("network error"), true) {
		t.Fatal("network error was treated as an authorization failure")
	}
	current.cfg.Headers = map[string]string{"Authorization": "Bearer synthetic"}
	if m.markSignInRequired(current, err, true) {
		t.Fatal("static credentials incorrectly enabled OAuth sign-in")
	}
	current.cfg.Headers = nil
	var notifications atomic.Int32
	m.OnChange(func() { notifications.Add(1) })
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			m.markSignInRequired(current, err, true)
			_ = m.Snapshot()
		})
	}
	wg.Wait()
	if !errors.Is(current.err, ErrSignInRequired) || notifications.Load() != 1 {
		t.Fatalf("concurrent failures did not share one state transition: notifications=%d", notifications.Load())
	}
	if current.authMetadataURL != "https://mcp.example.test/prm" {
		t.Fatal("cross-origin challenge replaced trusted discovery metadata")
	}
}

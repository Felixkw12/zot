package mcp

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
)

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

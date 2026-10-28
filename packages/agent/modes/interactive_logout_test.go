package modes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider/auth"
)

func TestLogoutStoredCustomProvider(t *testing.T) {
	for _, target := range []string{"removed-custom", "all", ""} {
		t.Run("target="+target, func(t *testing.T) {
			store := auth.NewStore(filepath.Join(t.TempDir(), "auth.json"))
			if err := store.SetAPIKey("removed-custom", "test-key"); err != nil {
				t.Fatal(err)
			}
			if err := store.SetEndpointCredential("another-custom", "http://localhost:8080", ""); err != nil {
				t.Fatal(err)
			}
			i := NewInteractive(InteractiveConfig{AuthManager: auth.NewManager(store)})
			i.openLogoutDialog()
			found := false
			for _, item := range i.logoutDialog.items {
				if item.target == "removed-custom" {
					found = true
				}
			}
			if !found {
				t.Fatal("stored custom provider missing from logout picker")
			}

			i.doLogout(target)
			if i.statusErr != "" {
				t.Fatalf("logout failed: %s", i.statusErr)
			}
			creds, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if creds.Has("removed-custom") {
				t.Fatal("custom credentials remain after logout")
			}
			if want := target == "removed-custom"; creds.Has("another-custom") != want {
				t.Fatalf("other custom credentials present = %v, want %v", creds.Has("another-custom"), want)
			}
		})
	}
}

func TestLogoutOpenAICredentialSeparation(t *testing.T) {
	for _, target := range []string{"openai", "openai-codex", "all"} {
		t.Run(target, func(t *testing.T) {
			store := auth.NewStore(filepath.Join(t.TempDir(), "auth.json"))
			if err := store.SetAPIKey("openai", "test-key"); err != nil {
				t.Fatal(err)
			}
			if err := store.SetOAuth("openai", auth.OAuthToken{AccessToken: "test-token"}); err != nil {
				t.Fatal(err)
			}
			i := NewInteractive(InteractiveConfig{AuthManager: auth.NewManager(store)})
			i.doLogout(target)
			if i.statusErr != "" {
				t.Fatalf("logout failed: %s", i.statusErr)
			}
			creds, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if got, want := creds.OpenAI.APIKey != "", target == "openai-codex"; got != want {
				t.Fatalf("API key present = %v, want %v", got, want)
			}
			if got, want := creds.OpenAI.OAuth != nil, target == "openai"; got != want {
				t.Fatalf("OAuth present = %v, want %v", got, want)
			}
		})
	}
}

func TestLogoutUnknownProvider(t *testing.T) {
	store := auth.NewStore(filepath.Join(t.TempDir(), "auth.json"))
	i := NewInteractive(InteractiveConfig{AuthManager: auth.NewManager(store)})
	i.doLogout("not-stored")
	if i.statusErr != "unknown provider: not-stored" {
		t.Fatalf("unexpected error: %q", i.statusErr)
	}
}

func TestLogoutUnreadableStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("invalid JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"removed-custom", "all"} {
		i := NewInteractive(InteractiveConfig{AuthManager: auth.NewManager(auth.NewStore(path))})
		i.doLogout(target)
		if !strings.Contains(i.statusErr, "read auth store:") {
			t.Fatalf("target %q: unexpected error: %q", target, i.statusErr)
		}
	}
}

package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/keymap"
)

func TestSaveConfigPreservesKeymapForPartialConfig(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())

	keymap := map[string]string{"ctrl+shift+g": "/git-tools:status"}
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ConfigPath(), []byte(`{"theme":"dark","keymap":{"ctrl+shift+g":"/git-tools:status"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(Config{Theme: "light"}); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Keymap) != 1 || got.Keymap["ctrl+shift+g"].Command != keymap["ctrl+shift+g"] {
		t.Fatalf("keymap = %#v, want %#v", got.Keymap, keymap)
	}
	if got.Theme != "light" {
		t.Fatalf("theme = %q, want %q", got.Theme, "light")
	}
}

func TestSaveConfigRejectsInvalidExistingConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"invalid JSON", `{"theme":"dark",`},
		{"unknown binding field", `{"keymap":{"ctrl+s":"/settings","ctrl+n":{"action":"next","context":"navigation","typo":true}}}`},
		{"wrong binding type", `{"keymap":{"ctrl+n":42}}`},
		{"invalid codemode settings", `{"codemode":{"mode":"invalid"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZOT_HOME", t.TempDir())
			original := []byte(tc.input)
			if err := os.WriteFile(ConfigPath(), original, 0o600); err != nil {
				t.Fatal(err)
			}
			saveErr := SaveConfig(Config{Theme: "light"})
			after, err := os.ReadFile(ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, original) {
				t.Fatal("saving overwrote the invalid existing config")
			}
			if saveErr == nil || !strings.Contains(saveErr.Error(), "read existing config before saving") {
				t.Fatalf("save error = %v, want contextual load error", saveErr)
			}
		})
	}
}

func TestSaveConfigRejectsExistingConfigReadError(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := os.Mkdir(ConfigPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(ConfigPath(), "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := SaveConfig(Config{Theme: "light"})
	if err == nil || !strings.Contains(err.Error(), "read existing config before saving") {
		t.Fatalf("save error = %v, want contextual read error", err)
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "keep" {
		t.Fatalf("existing directory contents changed: %q, %v", content, err)
	}
}

func TestSaveConfigPreservesNavigationKeymap(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	input := []byte(`{"keymap":{"ctrl+s":"/settings","ctrl+n":{"action":"next","context":"navigation"}}}`)
	if err := os.WriteFile(ConfigPath(), input, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(Config{Theme: "light"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	want := keymap.Binding{Action: "next", Context: "navigation"}
	if got.Keymap["ctrl+n"] != want || got.Keymap["ctrl+s"].Command != "/settings" {
		t.Fatalf("settings save changed keymap: %#v", got.Keymap)
	}
}

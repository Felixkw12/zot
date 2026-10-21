package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestAddServerPreservesExistingEntriesAndRefusesDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "mcp.json")
	writeConfig(t, path, `{"other":true,"mcpServers":{"old-one":{"command":"old"}}}`)
	cfg := ServerConfig{URL: "https://example.test/mcp", Headers: map[string]string{"Authorization": "Bearer ${DOCS_TOKEN}"}}
	if err := AddServer(path, "docs", cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Other   bool                    `json:"other"`
		Servers map[string]ServerConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if !file.Other || file.Servers["old-one"].Command != "old" || !reflect.DeepEqual(file.Servers["docs"], cfg) {
		t.Fatalf("file=%s", data)
	}
	if !strings.Contains(string(data), "${DOCS_TOKEN}") {
		t.Fatal("environment reference was expanded")
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Fatalf("mode=%v", info.Mode().Perm())
		}
	}
	if err := AddServer(path, "old_one", ServerConfig{Command: "new"}); err == nil {
		t.Fatal("colliding name overwrote an entry")
	}
	if err := AddServer(path, "bad name", ServerConfig{Command: "x"}); err == nil {
		t.Fatal("invalid name accepted")
	}
	if err := AddServer(path, "web", ServerConfig{URL: "ftp://example.test"}); err == nil {
		t.Fatal("invalid URL accepted")
	}
	writeConfig(t, path, `{"mcpServers":`)
	if err := AddServer(path, "x", ServerConfig{Command: "x"}); err == nil {
		t.Fatal("malformed file overwritten")
	}
}

func TestSplitCommandLine(t *testing.T) {
	words, err := SplitCommandLine(`npx -y "@scope/server name" 'a b' c\ d ${HOME}`)
	if err != nil || !reflect.DeepEqual(words, []string{"npx", "-y", "@scope/server name", "a b", "c d", "${HOME}"}) {
		t.Fatalf("words=%q err=%v", words, err)
	}
	if _, err := SplitCommandLine(`npx "open`); err == nil {
		t.Fatal("unterminated quote accepted")
	}
}

func TestFieldEditsPreserveUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	writeConfig(t, path, `{"mcpServers":{"fs":{"command":"x","custom":{"keep":1},"exposure":"direct"}}}`)
	if err := SetServerEnabled(path, "fs", false); err != nil {
		t.Fatal(err)
	}
	if err := SetServerExposure(path, "fs", "deferred"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var file struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"command": "x", "custom": map[string]any{"keep": float64(1)}, "enabled": false}
	if !reflect.DeepEqual(file.Servers["fs"], want) {
		t.Fatalf("entry=%s", data)
	}
	if err := SetServerEnabled(path, "fs", true); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(path); strings.Contains(string(data), "enabled") {
		t.Fatalf("enable kept the field: %s", data)
	}
	if err := SetServerExposure(path, "fs", "bogus"); err == nil {
		t.Fatal("invalid exposure accepted")
	}
	if err := SetServerEnabled(path, "absent", true); err == nil {
		t.Fatal("edit of a missing server succeeded")
	}
	if err := RemoveServer(path, "fs"); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(path); strings.Contains(string(data), `"fs"`) {
		t.Fatalf("remove kept the entry: %s", data)
	}
}

func TestEditToleratesNullServersAndKeepsSymlinksAndModes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	writeConfig(t, path, `{"mcpServers":null}`)
	if err := AddServer(path, "x", ServerConfig{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	target := filepath.Join(dir, "dotfiles", "mcp.json")
	writeConfig(t, target, `{}`)
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := AddServer(link, "y", ServerConfig{Command: "y"}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(link); info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%v", info.Mode().Perm())
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), `"y"`) {
		t.Fatalf("target not updated: %s", data)
	}
}

func TestEnablingABareOverrideRemovesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	writeConfig(t, path, `{"mcpServers":{"shared":{"enabled":false}}}`)
	if err := SetServerEnabled(path, "shared", true); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "shared") {
		t.Fatalf("empty override kept: %s", data)
	}
}

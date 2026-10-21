package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigMergeExpansionAndIsolation(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	t.Setenv("ZOT_MCP_TEST_TOKEN", "synthetic-token")
	t.Setenv("ZOT_MCP_TEST_UNSET", "")
	if err := os.Unsetenv("ZOT_MCP_TEST_UNSET"); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, filepath.Join(home, "mcp.json"), `{"mcpServers":{"shared":{"command":"${ZOT_MCP_TEST_UNSET}"},"global":{"command":"global"}}}`)
	writeConfig(t, filepath.Join(project, ".zot", "mcp.json"), `{"mcpServers":{
 "shared":{"url":"https://example.test/mcp","headers":{"Authorization":"Bearer ${ZOT_MCP_TEST_TOKEN}"}},
 "disabled":{"enabled":false},
 "bad":{"command":"secret-prefix ${ZOT_MCP_TEST_UNSET}"},
 "wrong":42,
 "local":{"command":"local","args":["${ZOT_MCP_TEST_UNSET:-fallback}","$HOME"],"cwd":"sub"}
 }}`)
	cfg, errs := loadConfig(home, project)
	if len(errs) != 2 {
		t.Fatalf("errors = %v", errs)
	}
	for _, err := range errs {
		if strings.Contains(err.Error(), "secret-prefix") {
			t.Fatal("credential leaked")
		}
	}
	if cfg["shared"].Transport != "streamable-http" || cfg["shared"].Headers["Authorization"] != "Bearer synthetic-token" {
		t.Fatalf("shared = %+v", cfg["shared"])
	}
	if cfg["global"].Command != "global" || cfg["local"].Args[0] != "fallback" || cfg["local"].Args[1] != "$HOME" || cfg["local"].CWD != filepath.Join(project, "sub") {
		t.Fatal("merge or expansion failed")
	}
	if cfg["disabled"].Enabled == nil || *cfg["disabled"].Enabled {
		t.Fatal("disabled server lost")
	}
}

func TestConfigValidation(t *testing.T) {
	for _, entry := range []string{
		`{"command":"x","requestTimeout":-1}`,
		`{"transport":"invalid","command":"x"}`,
		`{"command":"x","url":"https://example.test"}`,
		`{"command":"x","exposure":"invalid"}`,
		`null`,
	} {
		t.Run(entry, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			writeConfig(t, filepath.Join(home, "mcp.json"), `{"mcpServers":{"bad":`+entry+`}}`)
			cfg, errs := loadConfig(home, project)
			if len(errs) != 1 || len(cfg) != 0 {
				t.Fatalf("cfg=%v errors=%v", cfg, errs)
			}
		})
	}
}

func TestConfigMalformedProjectStillValidatesGlobal(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	writeConfig(t, filepath.Join(home, "mcp.json"), `{"mcpServers":{"valid":{"command":"tool"},"a-b":{"command":"tool"},"a_b":{"command":"tool"}}}`)
	writeConfig(t, filepath.Join(project, ".zot", "mcp.json"), `{`)
	cfg, errs := loadConfig(home, project)
	if len(errs) != 2 || cfg["valid"].Command != "tool" || len(cfg) != 2 {
		t.Fatalf("cfg=%v errors=%v", cfg, errs)
	}
}

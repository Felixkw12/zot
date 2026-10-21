package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
)

type changingMCPSource struct {
	mu    sync.Mutex
	tools []core.Tool
}

func (s *changingMCPSource) Tools() []core.Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Tool(nil), s.tools...)
}
func (s *changingMCPSource) Reload(context.Context) []error                  { return nil }
func (s *changingMCPSource) Command(context.Context, string) (string, error) { return "", nil }
func (s *changingMCPSource) Close()                                          {}
func (s *changingMCPSource) set(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = nil
	for _, name := range names {
		s.tools = append(s.tools, toolStub{name})
	}
}

func TestContinuousAgentsFollowMCPChanges(t *testing.T) {
	mgr := extensions.New(t.TempDir(), t.TempDir(), "continuous", "openai", "gpt-4o-mini", nonInteractiveExtHooks{})
	t.Cleanup(func() { mgr.Stop(time.Millisecond) })
	source := &changingMCPSource{}
	source.set("mcp__s__old")
	mgr.AddBuiltin("mcp", source)
	r := Resolved{Provider: "openai", Credential: "synthetic-key", Model: "gpt-4o-mini", ToolRegistry: core.NewRegistry(toolStub{"read"})}
	r.MergeExtensionTools(&extToolAdapter{mgr: mgr})
	engine := newContinuousEngine(r, mgr, nil, func() *continuous.Service { return nil })
	build := func() *core.Agent {
		t.Helper()
		ag, err := engine.Build(context.Background(), continuous.Conversation{})
		if err != nil {
			t.Fatal(err)
		}
		return ag
	}
	first, second := build(), build()
	source.set("mcp__s__new")
	mgr.NotifyBuiltinChange()
	third := build()
	for _, ag := range []*core.Agent{first, second, third} {
		reg := ag.ToolRegistry()
		if reg["mcp__s__new"] == nil || reg["mcp__s__old"] != nil {
			t.Fatalf("stale registry: %v", reg)
		}
		for _, name := range []string{"read", "subagent", "handoff"} {
			if reg[name] == nil {
				t.Fatalf("host tool %s lost", name)
			}
		}
	}
	// Builds and notification callbacks must serialize their catalog snapshots.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := engine.Build(context.Background(), continuous.Conversation{})
			if err != nil {
				t.Error(err)
			}
			mgr.NotifyBuiltinChange()
		}()
	}
	wg.Wait()
	source.set()
	mgr.NotifyBuiltinChange()
	if first.ToolRegistry()["mcp__s__new"] != nil || build().ToolRegistry()["mcp__s__new"] != nil {
		t.Fatal("withdrawn MCP tool retained")
	}
}

func TestContinuousMCPFailuresAreNonFatal(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(auth.Close)
	config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"healthy": map[string]any{"url": mcpFixtureServer(t), "exposure": "direct"},
		"signin":  map[string]any{"url": auth.URL},
		"invalid": map[string]any{"transport": "invalid"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ZotHome(), "mcp.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := os.CreateTemp(t.TempDir(), "diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = diagnostics
	t.Cleanup(func() {
		os.Stderr = previous
		diagnostics.Close()
	})
	args := Args{Mode: ModePrint, Provider: "openai", Model: "gpt-4o-mini", APIKey: "synthetic-key", CWD: t.TempDir(), NoExt: true, NoSkill: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	engine, stop, err := loadContinuousEngine(ctx, args, nil, func() *continuous.Service { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	ag, err := engine.Build(ctx, continuous.Conversation{})
	if err != nil {
		t.Fatal(err)
	}
	if ag.ToolRegistry()["mcp__healthy__lookup"] == nil {
		t.Fatal("healthy MCP server lost")
	}
	data, err := os.ReadFile(diagnostics.Name())
	if err != nil || !strings.Contains(string(data), "signin") || !strings.Contains(string(data), "invalid") {
		t.Fatalf("diagnostics=%s err=%v", data, err)
	}
}

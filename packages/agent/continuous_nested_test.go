package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage/memory"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type conversationProbeTool struct{ name, text string }

func (p conversationProbeTool) Name() string          { return p.name }
func (conversationProbeTool) Description() string     { return "local fixture" }
func (conversationProbeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p conversationProbeTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: p.text}}}, nil
}

func TestContinuousNestedExtensionUsesParentAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires /bin/sh")
	}
	t.Setenv("ZOT_HOME", t.TempDir())
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' '{"type":"hello","name":"nested","version":"1","capabilities":["tools","call_tool"]}'
printf '%s\n' '{"type":"register_tool","name":"outer","description":"local fixture","schema":{"type":"object"}}'
printf '%s\n' '{"type":"ready"}'
while IFS= read -r line
do
 case "$line" in
 *'"type":"tool_call"'*)
  parent=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '%s\n' "{\"type\":\"call_tool\",\"id\":\"child\",\"parent_id\":\"$parent\",\"name\":\"probe\",\"args\":{}}"
  IFS= read -r result
  printf '%s\n' "$result" | sed "s/\"id\":\"child\"/\"id\":\"$parent\"/"
  ;;
 *'"type":"shutdown"'*) printf '%s\n' '{"type":"shutdown_ack"}'
  exit 0
  ;;
 esac
done
`
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extension.json"), []byte(`{"name":"nested","exec":"./run.sh"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mgr := extensions.New(t.TempDir(), t.TempDir(), "test", "", "", nil)
	if errs := mgr.LoadExplicit(ctx, []string{dir}); len(errs) != 0 {
		t.Fatal(errs)
	}
	defer mgr.Stop(time.Second)
	mgr.WaitForReady(time.Second)
	resolved := Resolved{Provider: "openai", Credential: "synthetic-key", Model: "gpt-4o-mini", ToolRegistry: core.Registry{"probe": conversationProbeTool{name: "probe", text: "X"}}}
	resolved.MergeExtensionTools(&extToolAdapter{mgr: mgr})
	engine := newContinuousEngine(resolved, mgr, nil, func() *continuous.Service { return nil })
	type built struct {
		name string
		ag   *core.Agent
		err  error
	}
	start := make(chan struct{})
	agents := make(chan built, 2)
	for _, name := range []string{"X", "Y"} {
		go func() {
			<-start
			ag, err := engine.Build(ctx, continuous.Conversation{ID: name})
			agents <- built{name, ag, err}
		}()
	}
	close(start)
	byName := map[string]*core.Agent{}
	for range 2 {
		select {
		case b := <-agents:
			if b.err != nil {
				t.Fatal(b.err)
			}
			b.ag.Tools["probe"] = conversationProbeTool{name: "probe", text: b.name}
			byName[b.name] = b.ag
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// Exercise both parents so a single last-built binding always fails,
	// regardless of the concurrent build order.
	for _, name := range []string{"X", "Y"} {
		got := byName[name].CallTool(ctx, "parent-"+name, "outer", json.RawMessage(`{}`), nil)
		if got.IsError || core.ToolResultText(got) != name {
			t.Fatalf("%s callback used another conversation: %+v", name, got)
		}
	}
	// The continuous caller must refuse spontaneous calls even with a live agent.
	out := extensionToolCaller(mgr, nil)(ctx, "nested", "probe", json.RawMessage(`{}`))
	if !out.IsError || !strings.Contains(out.Content[0].Text, "parent") {
		t.Fatalf("parentless result: %+v", out)
	}

	// Drive the same real extension through continuous executeCall, where
	// the parent's agent guard hooks are disabled during execution.
	rt, err := continuous.New(memory.Open())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	guarded := 0
	blocked := false
	svc, err := continuous.NewService(rt, continuous.EngineFunc(func(ctx context.Context, c continuous.Conversation) (*core.Agent, error) {
		ag, err := engine.Build(ctx, c)
		if err != nil {
			return nil, err
		}
		ag.Client = nestedFixtureClient{}
		ag.BeforeToolExecuteContext = func(ctx context.Context, call provider.ToolCallBlock) (bool, string, json.RawMessage) {
			if call.Name == "probe" {
				guarded++
				identity, ok := continuous.ToolCallFromContext(ctx)
				if !ok || identity.CallID != "parent/n/1" || identity.ConversationID != c.ID || core.ToolOperationKey(ctx) != identity.OperationKey() {
					t.Errorf("nested extension identity: %+v present=%v", identity, ok)
				}
				if mgr.ToolCallOrigin(call.ID) != "nested" {
					t.Error("nested extension origin missing")
				}
				return false, "nested guard denied", nil
			}
			return true, "", nil
		}
		return ag, nil
	}), continuous.ExecutionOptions{Sink: func(ev core.AgentEvent) {
		if e, ok := ev.(core.EvToolResult); ok && e.Name == "outer" {
			blocked = e.Result.IsError && strings.Contains(core.ToolResultText(e.Result), "nested guard denied")
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := rt.OpenRoot(ctx, "workspace", continuous.AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Submit(ctx, c.ID, "actor", "", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Step(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if guarded != 1 || !blocked {
		t.Fatalf("continuous extension guard bypass: guarded=%d blocked=%v", guarded, blocked)
	}
}

// No credentials or network are used by the continuous extension fixture.
type nestedFixtureClient struct{}

func (nestedFixtureClient) Name() string { return "fixture" }
func (nestedFixtureClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	stop := provider.StopToolUse
	content := []provider.Content{provider.ToolCallBlock{ID: "parent", Name: "outer", Arguments: json.RawMessage(`{}`)}}
	for _, message := range req.Messages {
		if message.Role == provider.RoleTool {
			stop = provider.StopEnd
			content = []provider.Content{provider.TextBlock{Text: "done"}}
		}
	}
	ch := make(chan provider.Event, 1)
	ch <- provider.EventDone{Stop: stop, Message: provider.Message{Role: provider.RoleAssistant, Content: content}}
	close(ch)
	return ch, nil
}

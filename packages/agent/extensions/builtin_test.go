package extensions

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type builtinFixture struct{ reloads, closes int }

func (*builtinFixture) Tools() []core.Tool                              { return []core.Tool{builtinFixtureTool{}} }
func (b *builtinFixture) Reload(context.Context) []error                { b.reloads++; return nil }
func (*builtinFixture) Command(context.Context, string) (string, error) { return "native status", nil }
func (b *builtinFixture) Close()                                        { b.closes++ }

type blockingBuiltin struct {
	builtinFixture
	started chan struct{}
	release chan struct{}
}

func (b *blockingBuiltin) Reload(context.Context) []error {
	b.reloads++
	close(b.started)
	<-b.release
	return nil
}

func TestBuiltinShutdownWinsOverReload(t *testing.T) {
	mgr := New(t.TempDir(), t.TempDir(), "test", "", "", &stubHooks{})
	source := &blockingBuiltin{started: make(chan struct{}), release: make(chan struct{})}
	mgr.AddBuiltin("native", source)
	reloaded := make(chan struct{})
	go func() { mgr.reloadBuiltin(context.Background(), "native"); close(reloaded) }()
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("reload did not start")
	}
	mgr.stopLifecycle()
	stopped := make(chan struct{})
	go func() { mgr.Stop(time.Millisecond); close(stopped) }()
	close(source.release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
	<-reloaded
	mgr.reloadBuiltin(context.Background(), "native")
	if source.reloads != 1 || source.closes != 1 {
		t.Fatalf("reloads=%d closes=%d", source.reloads, source.closes)
	}
}

type builtinFixtureTool struct{}

func (builtinFixtureTool) Name() string            { return "native_tool" }
func (builtinFixtureTool) Description() string     { return "native fixture" }
func (builtinFixtureTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (builtinFixtureTool) Deferred() bool          { return true }
func (builtinFixtureTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "native result"}}}, nil
}

func TestBuiltinToolsCommandsAndLifecycle(t *testing.T) {
	mgr := New(t.TempDir(), t.TempDir(), "test", "", "", &stubHooks{})
	source := &builtinFixture{}
	mgr.AddBuiltin("native", source)
	infos := mgr.Tools()
	if len(infos) != 1 || !infos[0].Deferred || !mgr.HasTool("native_tool") {
		t.Fatalf("tools=%+v", infos)
	}
	tool := NewTool(mgr, infos[0])
	if _, ok := tool.(builtinFixtureTool); !ok {
		t.Fatalf("native tool wrapped as %T", tool)
	}
	if !mgr.HasCommand("NATIVE") || mgr.CommandOwner("native") != "native" || len(mgr.Commands()) != 1 {
		t.Fatal("native command unavailable")
	}
	if !mgr.Commands()[0].Builtin {
		t.Fatal("native command lacks built-in source metadata")
	}
	result, err := mgr.Invoke(context.Background(), "native", "", time.Second)
	if err != nil || result.Display != "native status" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	output, err := mgr.InvokeTool(context.Background(), "native_tool", json.RawMessage(`{}`), time.Second)
	if err != nil || len(output.Content) != 1 || output.Content[0].Text != "native result" {
		t.Fatalf("output=%+v err=%v", output, err)
	}
	if _, err := mgr.Invoke(context.Background(), "native", "reload", time.Second); err != nil {
		t.Fatal(err)
	}
	if source.reloads != 1 {
		t.Fatalf("reload count=%d", source.reloads)
	}
	mgr.Stop(time.Millisecond)
	if source.closes != 1 {
		t.Fatalf("close count=%d", source.closes)
	}
}

func TestBuiltinReloadIsIndependentOfExtensions(t *testing.T) {
	mgr := New(t.TempDir(), t.TempDir(), "test", "", "", &stubHooks{})
	source := &builtinFixture{}
	other := &builtinFixture{}
	mgr.AddBuiltin("native", source)
	mgr.AddBuiltin("other", other)
	refreshes := 0
	mgr.SetOnReload(func() { refreshes++ })
	if _, err := mgr.Invoke(context.Background(), "native", "reload", time.Second); err != nil {
		t.Fatal(err)
	}
	if source.reloads != 1 || other.reloads != 0 || refreshes != 1 {
		t.Fatalf("target reloads=%d other reloads=%d registry refreshes=%d", source.reloads, other.reloads, refreshes)
	}
	stats := mgr.Reload(context.Background(), time.Millisecond)
	if len(stats.Errors) != 0 || source.reloads != 1 || other.reloads != 0 {
		t.Fatalf("extension reload touched built-ins: target=%d other=%d errors=%v", source.reloads, other.reloads, stats.Errors)
	}
	mgr.Stop(time.Millisecond)
}

func TestInstalledExtensionShadowsBuiltin(t *testing.T) {
	mgr := New(t.TempDir(), t.TempDir(), "test", "", "", &stubHooks{})
	source := &builtinFixture{}
	mgr.AddBuiltin("native", source)
	mgr.ext["native"] = &Extension{Manifest: Manifest{Name: "native"}}
	if len(mgr.Tools()) != 0 || mgr.HasTool("native_tool") || mgr.HasCommand("native") || len(mgr.Commands()) != 0 {
		t.Fatal("shadowed builtin exposed")
	}
	mgr.closeShadowedBuiltins()
	if source.reloads != 0 || source.closes != 1 {
		t.Fatalf("reloads=%d closes=%d", source.reloads, source.closes)
	}
	delete(mgr.ext, "native")
	if errs := mgr.reloadBuiltin(context.Background(), "native"); len(errs) != 0 || source.reloads != 1 {
		t.Fatal("builtin did not resume after extension removal")
	}
}

type staleFixture struct {
	builtinFixture
	changed bool
	calls   int
}

func (s *staleFixture) RefreshStale(context.Context) bool { s.calls++; return s.changed }

func TestRefreshBuiltinToolsRefreshesOnlyOnChange(t *testing.T) {
	mgr := New(t.TempDir(), t.TempDir(), "test", "", "", &stubHooks{})
	source := &staleFixture{}
	mgr.AddBuiltin("native", source)
	refreshes := 0
	mgr.SetOnReload(func() { refreshes++ })
	mgr.RefreshBuiltinTools(context.Background())
	if source.calls != 1 || refreshes != 0 {
		t.Fatalf("unchanged: calls=%d refreshes=%d", source.calls, refreshes)
	}
	source.changed = true
	mgr.RefreshBuiltinTools(context.Background())
	if refreshes != 1 {
		t.Fatalf("changed tools did not refresh the registry: %d", refreshes)
	}
	mgr.ext["native"] = &Extension{Manifest: Manifest{Name: "native"}}
	mgr.RefreshBuiltinTools(context.Background())
	if source.calls != 2 {
		t.Fatal("shadowed built-in was asked")
	}
	delete(mgr.ext, "native")
	mgr.Stop(time.Millisecond)
}

package continuous

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/continuous/storage/sqlite"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type nestedCrashStore struct {
	storage.Store
	boundary string
	after    bool
	fired    atomic.Bool
	cancel   context.CancelFunc
}

func (s *nestedCrashStore) Commit(ctx context.Context, m storage.Mutation) (storage.Commit, error) {
	for _, op := range m.Operations {
		var task Task
		if op.Delete || json.Unmarshal(op.Value, &task) != nil || task.Kind != TaskKindTool {
			continue
		}
		in, _, err := decodeTool(task)
		if err != nil || in.ParentTask == "" {
			continue
		}
		matches := s.boundary == "intent" && task.State == "pending" || s.boundary == "effect" && task.Effect == effectStarted || s.boundary == "result" && task.State == "terminal"
		if matches && s.fired.CompareAndSwap(false, true) {
			if s.after {
				if _, err := s.Store.Commit(ctx, m); err != nil {
					return storage.Commit{}, err
				}
			}
			s.cancel()
			return storage.Commit{}, context.Canceled
		}
	}
	return s.Store.Commit(ctx, m)
}

func TestNestedCrashMatrix(t *testing.T) {
	for _, backend := range []string{"memory", "journal", "sqlite"} {
		for _, boundary := range []string{"intent", "effect", "result"} {
			for _, after := range []bool{false, true} {
				name := backend + "/" + boundary + "/before"
				if after {
					name = backend + "/" + boundary + "/after"
				}
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					dir := filepath.Join(t.TempDir(), "store")
					var store storage.Store
					var err error
					switch backend {
					case "memory":
						store = newMemoryStore()
					case "journal":
						store, err = journal.Open(ctx, dir, journal.Options{Durability: storage.Process})
					case "sqlite":
						store, err = sqlite.Open(ctx, dir, sqlite.Options{Durability: storage.Process})
					}
					if err != nil {
						t.Fatal(err)
					}
					fault := &nestedCrashStore{Store: store, boundary: boundary, after: after, cancel: cancel}
					inner := &effectTool{name: "inner", replay: core.ReplayNever}
					outer := &nestedSecurityTool{effectTool: &effectTool{name: "outer", replay: core.ReplaySafe}}
					outer.execute = func(ctx context.Context) (core.ToolResult, error) {
						return core.ToolRuntimeFromContext(ctx).Call(ctx, "inner", json.RawMessage(`{}`)), nil
					}
					h := newHarness(t, fault, []scriptStep{{calls: []provider.ToolCallBlock{call("parent", "outer", `{}`)}}, {text: "done"}}, outer, inner)
					defer h.r.Close()
					c := h.root(t)
					if _, err := h.r.Submit(ctx, c.ID, "actor", "", "go"); err != nil {
						t.Fatal(err)
					}
					if _, _, err := h.svc.Step(ctx, c.ID); err == nil || !fault.fired.Load() {
						t.Fatalf("boundary not reached: %v", err)
					}
					run, _, err := h.svc.Step(context.Background(), c.ID)
					want := int32(1)
					if boundary == "effect" && after {
						want = 0
					}
					if err != nil || run.Outcome != "completed" || inner.calls.Load() != want {
						t.Fatalf("recovery: %+v %v effects=%d want=%d", run, err, inner.calls.Load(), want)
					}
					if report, err := h.r.CheckIntegrity(context.Background()); err != nil || !report.Valid {
						t.Fatalf("integrity: %+v %v", report, err)
					}
				})
			}
		}
	}
}

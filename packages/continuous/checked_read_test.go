package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/memory"
	"github.com/patriceckhart/zot/packages/core"
)

var errInjectedRead = errors.New("injected remote read failure")

// faultyStore wraps a local store with checked snapshots and lifecycle
// observation, like a remote backend. Once armed, checked reads of keys
// with the given prefix fail even though the records exist, and the first
// failure terminates the store as a remote backend would.
type faultyStore struct {
	storage.Store
	mu     sync.Mutex
	prefix string
	armed  bool
	err    error
	done   chan struct{}
}

func newFaultyStore() *faultyStore {
	return &faultyStore{Store: memory.Open(), done: make(chan struct{})}
}

func (f *faultyStore) arm(prefix string) {
	f.mu.Lock()
	f.prefix, f.armed = prefix, true
	f.mu.Unlock()
}

func (f *faultyStore) fail(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.armed || !strings.HasPrefix(key, f.prefix) {
		return nil
	}
	if f.err == nil {
		f.err = errInjectedRead
		close(f.done)
	}
	return errInjectedRead
}

func (f *faultyStore) Done() <-chan struct{} { return f.done }
func (f *faultyStore) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *faultyStore) Snapshot(ctx context.Context) (storage.Snapshot, error) {
	if err := f.Err(); err != nil {
		return nil, err
	}
	s, err := f.Store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return &faultySnapshot{Snapshot: s, f: f}, nil
}

func (f *faultyStore) Commit(ctx context.Context, m storage.Mutation) (storage.Commit, error) {
	if err := f.Err(); err != nil {
		return storage.Commit{}, err
	}
	return f.Store.Commit(ctx, m)
}

type faultySnapshot struct {
	storage.Snapshot
	f *faultyStore
}

// Legacy reads must not be used by the runtime: they report absence.
func (s *faultySnapshot) Get(key string) (json.RawMessage, bool) {
	if s.f.fail(key) != nil {
		return nil, false
	}
	return s.Snapshot.Get(key)
}

func (s *faultySnapshot) GetContext(ctx context.Context, key string) (json.RawMessage, bool, error) {
	if err := s.f.fail(key); err != nil {
		return nil, false, err
	}
	b, ok := s.Snapshot.Get(key)
	return b, ok, nil
}

func (s *faultySnapshot) PageContext(ctx context.Context, prefix, after string, limit int) ([]storage.Record, error) {
	if err := s.f.fail(prefix); err != nil {
		return nil, err
	}
	return s.Snapshot.Page(prefix, after, limit)
}

func TestCheckedReadFailureIsNotAbsence(t *testing.T) {
	ctx := context.Background()
	f := newFaultyStore()
	r, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c, err := r.OpenRoot(ctx, "workspace", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, c.ID, "actor", "first", "input"); err != nil {
		t.Fatal(err)
	}
	// An active chain exists. A failed read must not be mistaken for an idle
	// conversation that may be forked or busy-checked away.
	f.arm("chain/")
	if _, err := r.Fork(ctx, c.ID, 1, nil); !errors.Is(err, errInjectedRead) {
		t.Fatalf("fork with failed chain read: %v", err)
	}
}

func TestCheckedReadFailureBlocksSubmission(t *testing.T) {
	ctx := context.Background()
	f := newFaultyStore()
	r, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c, err := r.OpenRoot(ctx, "workspace", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, c.ID, "actor", "first", "input"); err != nil {
		t.Fatal(err)
	}
	f.arm("chain/")
	if _, err := r.Submit(ctx, c.ID, "actor", "second", "more"); !errors.Is(err, errInjectedRead) {
		t.Fatalf("admission with failed chain read: %v", err)
	}
}

func TestOverlayCheckedReads(t *testing.T) {
	f := newFaultyStore()
	base, err := f.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := pendingSnapshot(base, []storage.Operation{{Key: "x/1", Value: json.RawMessage(`1`)}})
	if _, ok := o.(storage.ContextSnapshot); !ok {
		t.Fatal("overlay does not offer checked reads")
	}
	f.arm("x/")
	if _, _, err := storage.Get(context.Background(), o, "x/2"); !errors.Is(err, errInjectedRead) {
		t.Fatalf("overlay get: %v", err)
	}
	if b, ok, err := storage.Get(context.Background(), o, "x/1"); err != nil || !ok || string(b) != "1" {
		t.Fatalf("pending value: %s %v %v", b, ok, err)
	}
	if _, err := storage.Page(context.Background(), o, "x/", "", 10); !errors.Is(err, errInjectedRead) {
		t.Fatalf("overlay page: %v", err)
	}
}

func TestHostRunReturnsStoreFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFaultyStore()
	r, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	client := &echoClient{delay: time.Hour}
	host, err := NewHost(r, echoEngine(client, core.NewRegistry()), HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.OpenRoot(ctx, "workspace", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, c.ID, "actor", "", "question"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	// Wait until the model request is in flight, then lose the store. The
	// request blocks for an hour unless execution is cancelled first.
	deadline := time.Now().Add(10 * time.Second)
	for client.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("request never started")
		}
		time.Sleep(time.Millisecond)
	}
	f.arm("")
	f.fail("")
	select {
	case err := <-done:
		if !errors.Is(err, errInjectedRead) {
			t.Fatalf("host returned %v, want store failure", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host did not stop after store failure")
	}
}

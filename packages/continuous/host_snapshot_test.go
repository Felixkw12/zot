package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

type snapshotReadCounter struct {
	storage.Store
	entryGets  int
	entryPages int
}

func (s *snapshotReadCounter) Snapshot(ctx context.Context) (storage.Snapshot, error) {
	snap, err := s.Store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return &countedSnapshot{Snapshot: snap, counter: s}, nil
}

type countedSnapshot struct {
	storage.Snapshot
	counter *snapshotReadCounter
}

func (s *countedSnapshot) GetContext(ctx context.Context, key string) (json.RawMessage, bool, error) {
	if strings.HasPrefix(key, "entry/") {
		s.counter.entryGets++
	}
	return storage.Get(ctx, s.Snapshot, key)
}

func (s *countedSnapshot) PageContext(ctx context.Context, prefix, after string, limit int) ([]storage.Record, error) {
	if strings.HasPrefix(prefix, "entry/") {
		s.counter.entryPages++
	}
	return storage.Page(ctx, s.Snapshot, prefix, after, limit)
}

func seedSnapshotEntries(t *testing.T, r *Runtime, n int) Conversation {
	t.Helper()
	ctx := context.Background()
	c, err := r.OpenRoot(ctx, "snapshot-bench", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := r.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.EntrySequence = uint64(n)
	ops := []storage.Operation{record("conversation/"+c.ID, c)}
	for i := 1; i <= n; i++ {
		ops = append(ops, record(entryKey(c.ID, uint64(i)), Entry{ID: fmt.Sprint(i), ConversationID: c.ID, Type: "user"}))
	}
	if err := r.commit(ctx, snap, "test.seed", ops...); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConversationSnapshotBatchesEntries(t *testing.T) {
	for _, n := range []int{0, 1, 14, 200, 1001} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s := &snapshotReadCounter{Store: newMemoryStore()}
			r, err := New(s)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { r.Close() })
			c := seedSnapshotEntries(t, r, n)
			s.entryGets, s.entryPages = 0, 0
			const limit = 200
			view, err := r.ConversationSnapshot(context.Background(), c.ID, limit)
			if err != nil {
				t.Fatal(err)
			}
			want := min(n, limit)
			if len(view.Entries) != want || view.More != (n > limit) {
				t.Fatalf("entries=%d more=%v, want %d %v", len(view.Entries), view.More, want, n > limit)
			}
			for i, e := range view.Entries {
				if e.ID != fmt.Sprint(n-want+i+1) {
					t.Fatal("snapshot did not return the newest entries in sequence")
				}
			}
			wantPages := 0
			if n > 0 {
				wantPages = 1
			}
			if s.entryGets != 0 || s.entryPages != wantPages {
				t.Fatalf("entry reads: %d gets, %d pages, want 0 gets, %d pages", s.entryGets, s.entryPages, wantPages)
			}
		})
	}
}

func TestConversationSnapshotEntryReadFailure(t *testing.T) {
	f := newFaultyStore()
	r, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c := seedSnapshotEntries(t, r, 3)
	f.arm("entry/")
	if _, err := r.ConversationSnapshot(context.Background(), c.ID, 10); !errors.Is(err, errInjectedRead) {
		t.Fatalf("entry read failure was hidden: %v", err)
	}
}

func TestConversationSnapshotRejectsBrokenEntries(t *testing.T) {
	for _, missing := range []bool{true, false} {
		t.Run(fmt.Sprint("missing=", missing), func(t *testing.T) {
			r, err := New(newMemoryStore())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { r.Close() })
			c := seedSnapshotEntries(t, r, 3)
			snap, err := r.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			op := storage.Operation{Key: entryKey(c.ID, 2), Delete: missing}
			if !missing {
				op.Value = json.RawMessage(`true`)
			}
			if err := r.commit(context.Background(), snap, "test.corrupt", op); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ConversationSnapshot(context.Background(), c.ID, 10); !errors.Is(err, storage.ErrCorrupt) {
				t.Fatalf("broken transcript was accepted: %v", err)
			}
		})
	}
}

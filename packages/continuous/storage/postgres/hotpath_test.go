package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestHotReadsSurviveUnrelatedCommits(t *testing.T) {
	s := openTest(t, uniqueName(t), func(o *Options) { o.HeartbeatInterval = time.Hour })
	ctx := context.Background()
	commit(t, s, put("stable/key", `1`), put("task/a", `2`))
	before, _ := s.Snapshot(ctx)
	if _, _, err := storage.Get(ctx, before, "stable/key"); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"task/", "pending/"} {
		if _, err := storage.Page(ctx, before, prefix, "", 10); err != nil {
			t.Fatal(err)
		}
	}
	commit(t, s, put("unrelated/key", `3`))
	after, _ := s.Snapshot(ctx)
	s.rgate <- struct{}{}
	func() {
		defer func() { <-s.rgate }()
		deadline, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if v, ok, err := storage.Get(deadline, after, "stable/key"); err != nil || !ok || string(v) != "1" {
			t.Fatalf("unchanged point read went back to database: %v", err)
		}
		for _, prefix := range []string{"task/", "pending/"} {
			rows, err := storage.Page(deadline, after, prefix, "", 10)
			if err != nil {
				t.Fatalf("unchanged page went back to database: %v", err)
			}
			if prefix == "task/" {
				if len(rows) != 1 || rows[0].Key != "task/a" || string(rows[0].Value) != "2" {
					t.Fatal("unchanged page has incorrect contents")
				}
				rows[0].Value[0] = '9'
			}
		}
	}()
	rows, err := storage.Page(ctx, after, "task/", "", 10)
	if err != nil || string(rows[0].Value) != "2" {
		t.Fatal("cached page aliases returned data")
	}
	commit(t, s, put("task/b", `4`), put("pending/a", `5`), put("stable/key", `6`))
	latest, _ := s.Snapshot(ctx)
	rows, err = storage.Page(ctx, latest, "task/", "", 10)
	if err != nil || len(rows) != 2 {
		t.Fatal("page cache hid a matching insertion")
	}
	if rows, err := storage.Page(ctx, latest, "pending/", "", 10); err != nil || len(rows) != 1 {
		t.Fatal("negative page cache hid an insertion")
	}
	if v, ok, err := storage.Get(ctx, latest, "stable/key"); err != nil || !ok || string(v) != "6" {
		t.Fatal("point cache hid a matching update")
	}
	if rows, err := storage.Page(ctx, before, "task/", "", 10); err != nil || len(rows) != 1 {
		t.Fatal("cache changed an old snapshot")
	}
	if v, ok, err := storage.Get(ctx, before, "stable/key"); err != nil || !ok || string(v) != "1" {
		t.Fatal("cache changed an old point read")
	}
}

func TestLiveScanAvoidsDatabase(t *testing.T) {
	s := openTest(t, uniqueName(t), func(o *Options) { o.HeartbeatInterval = time.Hour })
	ctx := context.Background()
	before, _ := s.Snapshot(ctx)
	first := commit(t, s, put("key", `1`))
	second := commit(t, s, put("key", `2`))
	s.rgate <- struct{}{}
	func() {
		defer func() { <-s.rgate }()
		deadline, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		for _, tc := range []struct{ after, want uint64 }{{before.Revision(), first.Revision}, {first.Revision, second.Revision}} {
			rows, err := s.Scan(deadline, tc.after, 1)
			if err != nil || len(rows) != 1 || rows[0].Revision != tc.want {
				t.Fatalf("live watch fetched an acknowledged commit from database: %v", err)
			}
			rows[0].Operations[0].Value[0] = '9'
		}
	}()
	rows, err := s.Scan(ctx, before.Revision(), 100)
	if err != nil || len(rows) != 2 || string(rows[0].Operations[0].Value) != "1" || string(rows[1].Operations[0].Value) != "2" {
		t.Fatal("recent commit cache aliases returned values")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Scan(cancelled, before.Revision(), 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached scan ignored cancellation: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, before.Revision(), 1); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("cached scan hid closure: %v", err)
	}
}

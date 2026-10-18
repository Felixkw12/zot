package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestRecordCacheIsolationAndBounds(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	value := json.RawMessage(`{"value":1}`)
	s.cacheRecord(1, "key", value)
	value[0] = 'x'
	got, found, cached, err := s.cachedRecord(ctx, 1, "key")
	if err != nil || !found || !cached || string(got) != `{"value":1}` {
		t.Fatalf("cache aliases input: %s %v %v %v", got, found, cached, err)
	}
	got[0] = 'x'
	got, _, _, _ = s.cachedRecord(ctx, 1, "key")
	if got[0] != '{' {
		t.Fatal("cache aliases returned value")
	}
	if _, _, cached, _ := s.cachedRecord(ctx, 2, "key"); cached {
		t.Fatal("cache leaked across snapshot revisions")
	}
	s.cacheRecord(2, "key", nil)
	if _, found, cached, err := s.cachedRecord(ctx, 2, "key"); err != nil || found || !cached {
		t.Fatal("negative lookup was not cached")
	}
	s.cacheRecord(3, "key", json.RawMessage(`null`))
	if got, found, cached, _ := s.cachedRecord(ctx, 3, "key"); !found || !cached || string(got) != "null" {
		t.Fatal("JSON null was treated as absence")
	}
	for i := 0; i < recordCacheEntries*2; i++ {
		s.cacheRecord(4, fmt.Sprint(i), nil)
		if len(s.records) > recordCacheEntries || s.recordBytes > recordCacheBytes {
			t.Fatal("cache exceeded bounds")
		}
	}
	large := bytes.Repeat([]byte("x"), recordCacheBytes/2)
	for i := 0; i < 3; i++ {
		s.cacheRecord(5, fmt.Sprint(i), large)
		if s.recordBytes > recordCacheBytes {
			t.Fatal("cache exceeded byte bound")
		}
	}
	s.cacheRecord(6, "oversized", bytes.Repeat([]byte("x"), recordCacheBytes+1))
	if _, _, cached, _ := s.cachedRecord(ctx, 6, "oversized"); cached {
		t.Fatal("oversized value was cached")
	}
	var size int
	for k, v := range s.records {
		size += len(k.key) + len(v)
	}
	if size != s.recordBytes {
		t.Fatal("cache byte accounting differs from retained payload")
	}
}

func TestRecordCacheTerminalFailure(t *testing.T) {
	s := &Store{}
	s.cacheRecord(1, "key", json.RawMessage(`1`))
	s.state, s.err = statePoisoned, storage.ErrFenced
	if _, _, _, err := s.cachedRecord(context.Background(), 1, "key"); !errors.Is(err, storage.ErrFenced) {
		t.Fatalf("cache hid terminal failure: %v", err)
	}
	s.cacheRecord(1, "other", json.RawMessage(`2`))
	if len(s.records) != 1 {
		t.Fatal("failed store continued populating cache")
	}
}

func TestRecordCacheConcurrent(t *testing.T) {
	s := &Store{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				s.cacheRecord(uint64(j), "key", json.RawMessage(`1`))
				s.cachedRecord(context.Background(), uint64(j), "key")
			}
		}()
	}
	wg.Wait()
}

func TestPagedRecordsSeedCache(t *testing.T) {
	s := openTest(t, uniqueName(t))
	commit(t, s, put("page/key", `1`))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, s.name, func(o *Options) { o.HeartbeatInterval = time.Hour })
	ctx := context.Background()
	view, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := storage.Page(ctx, view, "page/", "", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("page read failed: %v", err)
	}
	rows[0].Value[0] = '9'
	s.rgate <- struct{}{}
	defer func() { <-s.rgate }()
	deadline, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if v, ok, err := storage.Get(deadline, view, "page/key"); err != nil || !ok || string(v) != "1" {
		t.Fatalf("page did not seed an isolated cache entry: %v", err)
	}
}

func TestCachedPointReadsAvoidDatabase(t *testing.T) {
	s := openTest(t, uniqueName(t), func(o *Options) { o.HeartbeatInterval = time.Hour })
	ctx := context.Background()
	commit(t, s, put("key", `1`), put("page/key", `2`))
	view, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Warm both a point miss and a page result. Successful writes seed hits.
	if _, ok, err := storage.Get(ctx, view, "missing"); err != nil || ok {
		t.Fatal("warming negative lookup failed")
	}
	if _, err := storage.Page(ctx, view, "page/", "", 10); err != nil {
		t.Fatal(err)
	}
	// A second snapshot at the same revision shares the bounded cache.
	view, err = s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.rgate <- struct{}{}
	func() {
		defer func() { <-s.rgate }()
		deadline, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		for _, tc := range []struct{ key, want string }{{"key", "1"}, {"page/key", "2"}, {"missing", ""}} {
			value, ok, err := storage.Get(deadline, view, tc.key)
			if err != nil || ok != (tc.want != "") || string(value) != tc.want {
				t.Fatalf("cached read waited for database: %v", err)
			}
		}
	}()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := storage.Get(cancelled, view, "key"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached read ignored cancellation: %v", err)
	}
	commit(t, s, put("key", `3`), storage.Operation{Key: "page/key", Delete: true})
	latest, _ := s.Snapshot(ctx)
	if v, ok, err := storage.Get(ctx, latest, "key"); err != nil || !ok || string(v) != "3" {
		t.Fatal("cache returned stale value")
	}
	if _, ok, err := storage.Get(ctx, latest, "page/key"); err != nil || ok {
		t.Fatal("cache returned deleted value")
	}
	if v, ok, err := storage.Get(ctx, view, "key"); err != nil || !ok || string(v) != "1" {
		t.Fatal("write changed older snapshot")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.Get(ctx, view, "key"); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("cache hid closed store: %v", err)
	}
}

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestPageCacheKeysAndInvalidation(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	key := pageCacheKey{"task/", "task/b", 2}
	rows := []storage.Record{{Key: "task/c", Value: json.RawMessage(`1`)}}
	s.cachePageLocked(1, key, rows)
	rows[0].Value[0] = '9'
	for _, other := range []pageCacheKey{{"other/", "task/b", 2}, {"task/", "task/a", 2}, {"task/", "task/b", 3}} {
		if _, cached, _ := s.cachedPage(ctx, 1, other); cached {
			t.Fatal("page cache ignored a request parameter")
		}
	}
	// Changes at or below the exclusive cursor cannot change this page.
	s.advanceReadsLocked(storage.Commit{Revision: 2, Operations: []storage.Operation{{Key: "task/b", Delete: true}}})
	got, cached, err := s.cachedPage(ctx, 2, key)
	if err != nil || !cached || string(got[0].Value) != "1" {
		t.Fatal("unaffected page was not carried forward or aliases input")
	}
	// An old read must not displace a current cached page.
	s.cachePageLocked(1, key, nil)
	if _, cached, _ := s.cachedPage(ctx, 2, key); !cached {
		t.Fatal("old snapshot replaced current page")
	}
	s.advanceReadsLocked(storage.Commit{Revision: 3, Operations: []storage.Operation{{Key: "task/z", Delete: true}}})
	if _, cached, _ := s.cachedPage(ctx, 3, key); cached {
		t.Fatal("matching delete failed to invalidate page")
	}
	if len(s.pages) != 0 || s.pageItems != 0 || s.pageBytes != 0 {
		t.Fatal("invalidated page retained budget")
	}
	s.cachePageLocked(3, key, nil)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.cachedPage(cancelled, 3, key); !errors.Is(err, context.Canceled) {
		t.Fatal("cached page ignored cancellation")
	}
	s.state, s.err = statePoisoned, storage.ErrFenced
	if _, _, err := s.cachedPage(ctx, 3, key); !errors.Is(err, storage.ErrFenced) {
		t.Fatal("cached page hid store failure")
	}
}

func TestPageCacheBounds(t *testing.T) {
	s := &Store{}
	for i := 0; i < pageCacheItems*2; i++ {
		s.cachePageLocked(1, pageCacheKey{prefix: fmt.Sprint(i)}, nil)
		if s.pageItems > pageCacheItems || s.pageBytes > pageCacheBytes {
			t.Fatal("negative pages exceeded cache bounds")
		}
	}
	large := []storage.Record{{Key: "k", Value: bytes.Repeat([]byte("x"), pageCacheBytes/2)}}
	for i := 0; i < 3; i++ {
		s.cachePageLocked(1, pageCacheKey{prefix: fmt.Sprint(i)}, large)
		if s.pageBytes > pageCacheBytes || s.pageItems > pageCacheItems {
			t.Fatal("large pages exceeded cache bounds")
		}
	}
	tooMany := make([]storage.Record, pageCacheItems)
	s.cachePageLocked(1, pageCacheKey{prefix: "oversized"}, tooMany)
	if _, cached, _ := s.cachedPage(context.Background(), 1, pageCacheKey{prefix: "oversized"}); cached {
		t.Fatal("oversized page was cached")
	}
	var size, items int
	for _, page := range s.pages {
		size += page.bytes
		items += len(page.rows) + 1
	}
	if s.pageBytes != size || s.pageItems != items {
		t.Fatal("page budget differs from retained payload")
	}
}

func TestRecentCommitCacheBoundsAndGaps(t *testing.T) {
	s := &Store{}
	for i := 1; i <= recentCommitEntries+3; i++ {
		s.cacheCommitLocked(storage.Commit{Revision: uint64(i), Operations: []storage.Operation{put("key", `1`)}})
		if len(s.recent) > recentCommitEntries || s.recentBytes > recentCommitBytes {
			t.Fatal("recent commits exceeded cache bounds")
		}
	}
	high := uint64(recentCommitEntries + 3)
	if _, cached := s.cachedCommitsLocked(0, high, 1); cached {
		t.Fatal("expired history cursor was served from cache")
	}
	if got, cached := s.cachedCommitsLocked(high-1, high, 10); !cached || len(got) != 1 || got[0].Revision != high {
		t.Fatal("recent history was not cached")
	}
	s.cacheCommitLocked(storage.Commit{Revision: high + 1, Operations: []storage.Operation{{Key: "large", Value: bytes.Repeat([]byte("x"), recentCommitBytes+1)}}})
	if len(s.recent) != 0 || s.recentBytes != 0 {
		t.Fatal("oversized commit did not clear recent history")
	}
	s.cacheCommitLocked(storage.Commit{Revision: high + 2})
	if _, cached := s.cachedCommitsLocked(high, high+2, 2); cached {
		t.Fatal("recent cache hid a history gap")
	}
	s.cacheCommitLocked(storage.Commit{Revision: high + 4})
	if len(s.recent) != 1 {
		t.Fatal("noncontiguous commit did not reset recent history")
	}
}

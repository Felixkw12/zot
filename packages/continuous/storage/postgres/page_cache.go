package postgres

import (
	"bytes"
	"context"
	"strings"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

const pageCacheBytes = 4 << 20
const pageCacheItems = 1024

type pageCacheKey struct {
	prefix string
	after  string
	limit  int
}

type cachedPage struct {
	revision uint64
	rows     []storage.Record
	bytes    int
}

func cloneRecords(rows []storage.Record) []storage.Record {
	out := make([]storage.Record, len(rows))
	for i, row := range rows {
		out[i] = storage.Record{Key: row.Key, Value: bytes.Clone(row.Value)}
	}
	return out
}

func (s *Store) cachedPage(ctx context.Context, revision uint64, key pageCacheKey) ([]storage.Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.terminalLocked(); err != nil {
		return nil, false, err
	}
	page, ok := s.pages[key]
	if !ok || page.revision != revision {
		return nil, false, nil
	}
	return cloneRecords(page.rows), true, nil
}

func (s *Store) cachePageLocked(revision uint64, key pageCacheKey, rows []storage.Record) {
	if s.state != stateOpen {
		return
	}
	size := len(key.prefix) + len(key.after)
	for _, row := range rows {
		size += len(row.Key) + len(row.Value)
	}
	items := len(rows) + 1
	if size > pageCacheBytes || items > pageCacheItems {
		return
	}
	if old, ok := s.pages[key]; ok {
		// A slow read of an old snapshot must not evict a newer page.
		if old.revision > revision {
			return
		}
		s.pageBytes -= old.bytes
		s.pageItems -= len(old.rows) + 1
		delete(s.pages, key)
	}
	if s.pageBytes+size > pageCacheBytes || s.pageItems+items > pageCacheItems {
		s.pages = nil
		s.pageBytes, s.pageItems = 0, 0
	}
	if s.pages == nil {
		s.pages = make(map[pageCacheKey]cachedPage)
	}
	s.pages[key] = cachedPage{revision: revision, rows: cloneRecords(rows), bytes: size}
	s.pageBytes += size
	s.pageItems += items
}

// advancePagesLocked invalidates conservatively on any matching operation,
// including inserts or deletions beyond the end of a limited page.
func (s *Store) advancePagesLocked(c storage.Commit) {
	for key, page := range s.pages {
		if page.revision != c.Revision-1 {
			continue
		}
		changed := false
		for _, op := range c.Operations {
			if op.Key > key.after && strings.HasPrefix(op.Key, key.prefix) {
				changed = true
				break
			}
		}
		if changed {
			s.pageBytes -= page.bytes
			s.pageItems -= len(page.rows) + 1
			delete(s.pages, key)
		} else {
			page.revision = c.Revision
			s.pages[key] = page
		}
	}
}

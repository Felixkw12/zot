package postgres

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Bound both payload bytes and entry count, including negative lookups.
// Clearing a full cache is intentionally simpler than maintaining an LRU.
const recordCacheBytes = 4 << 20
const recordCacheEntries = 1024

type recordCacheKey struct {
	revision uint64
	key      string
}

// cachedRecord returns only validated values at exactly the requested
// revision. Cache hits still observe caller cancellation and store failure.
func (s *Store) cachedRecord(ctx context.Context, revision uint64, key string) (json.RawMessage, bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.terminalLocked(); err != nil {
		return nil, false, false, err
	}
	value, cached := s.records[recordCacheKey{revision, key}]
	return bytes.Clone(value), value != nil, cached, nil
}

func (s *Store) cacheRecord(revision uint64, key string, value json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheRecordLocked(revision, key, value)
}

// cacheRecordLocked requires s.mu. A nil value represents verified absence.
// Callers may seed acknowledged writes or successful, validated reads only.
func (s *Store) cacheRecordLocked(revision uint64, key string, value json.RawMessage) {
	if s.state != stateOpen {
		return
	}
	size := len(key) + len(value)
	if size > recordCacheBytes {
		return
	}
	k := recordCacheKey{revision, key}
	if previous, exists := s.records[k]; exists {
		s.recordBytes -= len(key) + len(previous)
		delete(s.records, k)
	}
	if len(s.records) >= recordCacheEntries || s.recordBytes+size > recordCacheBytes {
		s.records = nil
		s.recordBytes = 0
	}
	if s.records == nil {
		s.records = make(map[recordCacheKey]json.RawMessage)
	}
	s.records[k] = bytes.Clone(value)
	s.recordBytes += size
}

// advanceReadsLocked carries unchanged reads across an acknowledged commit.
// The exclusive writer knows every changed key. Older snapshots still fall
// back to immutable database versions when their cache entries are moved.
func (s *Store) advanceReadsLocked(c storage.Commit) {
	if s.state != stateOpen {
		return
	}
	changed := make(map[string]bool, len(c.Operations))
	for _, op := range c.Operations {
		changed[op.Key] = true
	}
	for key, value := range s.records {
		if key.revision == c.Revision-1 && !changed[key.key] {
			next := recordCacheKey{c.Revision, key.key}
			// A reader can populate the next revision only after it is
			// published, which happens under this same lock.
			delete(s.records, key)
			s.records[next] = value
		}
	}
	s.advancePagesLocked(c)
}

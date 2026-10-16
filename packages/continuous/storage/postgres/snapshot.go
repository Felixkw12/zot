package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/internal/state"
)

// legacyReadTimeout bounds a compatibility read that has no caller context.
const legacyReadTimeout = 30 * time.Second

// bytea returns a non-nil copy, since pgx encodes a nil slice as SQL NULL.
func bytea(s string) []byte { return append([]byte{}, s...) }

type snapshot struct {
	s        *Store
	revision uint64
}

// Snapshot captures the latest acknowledged revision. It holds no
// transaction and copies nothing: later commits are invisible because
// record versions above the revision are filtered out.
func (s *Store) Snapshot(ctx context.Context) (storage.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.terminalLocked(); err != nil {
		return nil, err
	}
	return &snapshot{s: s, revision: s.revision}, nil
}

func (v *snapshot) Revision() uint64 { return v.revision }

// Get is a compatibility adapter. It cannot report a database error, so an
// error poisons the store and the record reads as absent. Use GetContext
// or storage.Get.
func (v *snapshot) Get(key string) (json.RawMessage, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), legacyReadTimeout)
	defer cancel()
	b, ok, err := v.GetContext(ctx, key)
	if err != nil {
		if v.s.terminal() == nil {
			v.s.fail(fmt.Errorf("continuous postgres legacy read failed: %w", err))
		}
		return nil, false
	}
	return b, ok
}

// Page is a compatibility adapter for PageContext.
func (v *snapshot) Page(prefix, after string, limit int) ([]storage.Record, error) {
	ctx, cancel := context.WithTimeout(context.Background(), legacyReadTimeout)
	defer cancel()
	return v.PageContext(ctx, prefix, after, limit)
}

// read runs fn on the reader connection with admitted-work semantics: the
// caller bounds only the wait for the connection. A database or validation
// failure poisons the store. A caller that cancelled after admission gets
// its context error once the read has drained.
func (v *snapshot) read(ctx context.Context, fn func(context.Context, *pgx.Conn) error) error {
	s := v.s
	if err := s.enter(ctx, s.rgate); err != nil {
		return err
	}
	defer func() { <-s.rgate }()
	opCtx, cancel := s.op()
	defer cancel()
	if err := fn(opCtx, s.reader); err != nil {
		if t := s.terminal(); t != nil {
			return t
		}
		return s.fail(err)
	}
	if t := s.terminal(); t != nil {
		return t
	}
	return ctx.Err()
}

// GetContext reads the newest version of key at or below the snapshot.
func (v *snapshot) GetContext(ctx context.Context, key string) (json.RawMessage, bool, error) {
	if key == "" || len(key) > maxKey {
		return nil, false, ctx.Err()
	}
	if v.revision > math.MaxInt64 {
		return nil, false, storage.ErrCursor
	}
	var out json.RawMessage
	var found bool
	err := v.read(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		var deleted bool
		var value []byte
		err := conn.QueryRow(ctx, `SELECT deleted, value
FROM zot_continuous.records
WHERE store_id = $1
  AND key_digest = $2
  AND key = $3
  AND revision <= $4
ORDER BY revision DESC
LIMIT 1`, v.s.storeID, digest(key), bytea(key), int64(v.revision)).Scan(&deleted, &value)
		if err == pgx.ErrNoRows {
			return nil
		}
		if err != nil {
			return safeError("read record", err)
		}
		if deleted {
			if value != nil {
				return corrupt("tombstone with value")
			}
			return nil
		}
		if value == nil || !json.Valid(value) {
			return corrupt("record value is not valid JSON")
		}
		out, found = bytes.Clone(value), true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, found, nil
}

// prefixUpper returns the exclusive upper bound of byte strings starting
// with b, or nil when there is none.
func prefixUpper(b []byte) []byte {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0xff {
			out := append([]byte(nil), b[:i+1]...)
			out[i]++
			return out
		}
	}
	return nil
}

// PageContext lists live records with prefix, after the exclusive cursor,
// in byte order, as of the snapshot.
func (v *snapshot) PageContext(ctx context.Context, prefix, after string, limit int) ([]storage.Record, error) {
	if limit < 1 || limit > state.MaxPage {
		return nil, fmt.Errorf("page limit must be between 1 and %d", state.MaxPage)
	}
	if v.revision > math.MaxInt64 {
		return nil, storage.ErrCursor
	}
	var lower, upper any
	if prefix != "" {
		b := keyPrefix(prefix)
		lower = b
		if u := prefixUpper(b); u != nil {
			upper = u
		}
	}
	var out []storage.Record
	err := v.read(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		out = out[:0]
		rows, err := conn.Query(ctx, `SELECT key, value
FROM (
    SELECT DISTINCT ON (key) key, deleted, value
    FROM zot_continuous.records
    WHERE store_id = $1
      AND revision <= $2
      AND key > $3
      AND ($4::bytea IS NULL OR key_prefix >= $4)
      AND ($5::bytea IS NULL OR key_prefix < $5)
      AND substring(key FROM 1 FOR octet_length($6::bytea)) = $6
    ORDER BY key, revision DESC
) AS visible
WHERE NOT deleted
ORDER BY key
LIMIT $7`, v.s.storeID, int64(v.revision), bytea(after), lower, upper, bytea(prefix), int32(limit))
		if err != nil {
			return safeError("page records", err)
		}
		defer rows.Close()
		previous := after
		for rows.Next() {
			var key, value []byte
			if err := rows.Scan(&key, &value); err != nil {
				return safeError("page records", err)
			}
			k := string(key)
			if k <= previous || !bytes.HasPrefix(key, []byte(prefix)) {
				return corrupt("page out of order")
			}
			if value == nil || !json.Valid(value) {
				return corrupt("record value is not valid JSON")
			}
			previous = k
			out = append(out, storage.Record{Key: k, Value: bytes.Clone(value)})
		}
		if err := rows.Err(); err != nil {
			return safeError("page records", err)
		}
		if len(out) > limit {
			return corrupt("page exceeds limit")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []storage.Record{}
	}
	return out, nil
}

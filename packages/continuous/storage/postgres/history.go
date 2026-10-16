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

// Scan returns up to limit commits after the cursor, in revision order,
// including acquisition and other zero-operation commits. It fetches
// bounded headers first, then the operations of exactly those revisions,
// and validates the result before returning any of it.
func (s *Store) Scan(ctx context.Context, after uint64, limit int) ([]storage.Commit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if err := s.terminalLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	high := s.revision
	s.mu.Unlock()
	if after > high || after > math.MaxInt64 {
		return nil, storage.ErrCursor
	}
	if limit < 1 || limit > state.MaxPage {
		return nil, fmt.Errorf("scan limit must be between 1 and %d", state.MaxPage)
	}
	if after == high {
		return []storage.Commit{}, nil
	}
	view := &snapshot{s: s, revision: high}
	var out []storage.Commit
	err := view.read(ctx, func(ctx context.Context, conn *pgx.Conn) error {
		var err error
		out, err = scanCommits(ctx, conn, s.storeID, after, high, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func scanCommits(ctx context.Context, conn *pgx.Conn, id int32, after, high uint64, limit int) ([]storage.Commit, error) {
	rows, err := conn.Query(ctx, `SELECT revision, epoch, commit_schema, committed_time, actor, operation_count
FROM zot_continuous.commits
WHERE store_id = $1 AND revision > $2 AND revision <= $3
ORDER BY revision
LIMIT $4`, id, int64(after), int64(high), int32(limit))
	if err != nil {
		return nil, safeError("scan history", err)
	}
	var commits []storage.Commit
	var counts []int32
	var revisions []int64
	for rows.Next() {
		var revision, epoch int64
		var schema, count int32
		var ts string
		var actor []byte
		if err := rows.Scan(&revision, &epoch, &schema, &ts, &actor, &count); err != nil {
			rows.Close()
			return nil, safeError("scan history", err)
		}
		t, perr := time.Parse(time.RFC3339Nano, ts)
		if perr != nil || count < 0 {
			rows.Close()
			return nil, corrupt("invalid commit header")
		}
		commits = append(commits, storage.Commit{Schema: int(schema), Revision: uint64(revision), Epoch: uint64(epoch), Time: t, Actor: string(actor), Operations: []storage.Operation{}})
		counts = append(counts, count)
		revisions = append(revisions, revision)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, safeError("scan history", err)
	}
	want := min(uint64(limit), high-after)
	if uint64(len(commits)) != want {
		return nil, corrupt("missing commit headers")
	}
	index := make(map[int64]int, len(revisions))
	for i, r := range revisions {
		index[r] = i
	}
	rows, err = conn.Query(ctx, `SELECT revision, ordinal, key, key_digest, key_prefix, deleted, value
FROM zot_continuous.records
WHERE store_id = $1 AND revision = ANY($2::bigint[])
ORDER BY revision, ordinal`, id, revisions)
	if err != nil {
		return nil, safeError("scan history", err)
	}
	for rows.Next() {
		var revision int64
		var ordinal int32
		var key, keyDigest, prefix, value []byte
		var deleted bool
		if err := rows.Scan(&revision, &ordinal, &key, &keyDigest, &prefix, &deleted, &value); err != nil {
			rows.Close()
			return nil, safeError("scan history", err)
		}
		i, ok := index[revision]
		if !ok || int(ordinal) != len(commits[i].Operations) {
			rows.Close()
			return nil, corrupt("invalid operation ordinal")
		}
		k := string(key)
		if !bytes.Equal(keyDigest, digest(k)) || !bytes.Equal(prefix, keyPrefix(k)) {
			rows.Close()
			return nil, corrupt("operation key index mismatch")
		}
		op := storage.Operation{Key: k, Delete: deleted}
		if deleted {
			if value != nil {
				rows.Close()
				return nil, corrupt("tombstone with value")
			}
		} else {
			if value == nil {
				rows.Close()
				return nil, corrupt("record without value")
			}
			op.Value = json.RawMessage(bytes.Clone(value))
		}
		commits[i].Operations = append(commits[i].Operations, op)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, safeError("scan history", err)
	}
	previousEpoch := uint64(0)
	for i := range commits {
		c := commits[i]
		if int32(len(c.Operations)) != counts[i] {
			return nil, corrupt("operation count mismatch")
		}
		if i > 0 {
			previousEpoch = commits[i-1].Epoch
		}
		if err := state.ValidateCommit(c, after+uint64(i)+1, previousEpoch); err != nil {
			return nil, err
		}
		if c.Epoch > c.Revision {
			return nil, corrupt("epoch exceeds revision")
		}
	}
	return commits, nil
}

// Wait returns once a revision after the cursor is acknowledged. It runs no
// query and is cancellable at any time. Cancelling it never cancels
// admitted work. Scan followed by Wait is gap-free.
func (s *Store) Wait(ctx context.Context, after uint64) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		if err := s.terminalLocked(); err != nil {
			s.mu.Unlock()
			return err
		}
		if after > s.revision {
			s.mu.Unlock()
			return storage.ErrCursor
		}
		if after < s.revision {
			s.mu.Unlock()
			return nil
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

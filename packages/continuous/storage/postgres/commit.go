package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/internal/state"
)

// Commit appends one atomic commit. Validation, fencing, and conflict
// errors leave the store open. Once the transaction is admitted, the
// caller's context no longer interrupts it: a successful acknowledgement is
// returned even if the caller cancelled meanwhile, and any database failure
// terminates the store. An unknown COMMIT outcome matches
// storage.ErrCommitUnknown and is never retried.
//
// With defaults, a call may wait up to 15 seconds for the connection and
// then up to OperationTimeout for the admitted transaction.
func (s *Store) Commit(ctx context.Context, m storage.Mutation) (storage.Commit, error) {
	if err := s.enter(ctx, s.wgate); err != nil {
		return storage.Commit{}, err
	}
	defer func() { <-s.wgate }()
	s.mu.Lock()
	revision, epoch := s.revision, s.epoch
	s.mu.Unlock()
	if m.Epoch != epoch {
		return storage.Commit{}, storage.ErrFenced
	}
	if m.ExpectedRevision != revision {
		return storage.Commit{}, storage.ErrConflict
	}
	if revision >= math.MaxInt64 {
		return storage.Commit{}, fmt.Errorf("store revision exhausted")
	}
	if err := validateOps(m.Actor, m.Operations); err != nil {
		return storage.Commit{}, err
	}
	c := cloneCommit(storage.Commit{Schema: 1, Revision: revision + 1, Epoch: epoch, Time: time.Now().UTC(), Actor: m.Actor, Operations: m.Operations})
	b, err := json.Marshal(c)
	if err != nil {
		return storage.Commit{}, err
	}
	if len(b) > state.MaxCommitBytes {
		return storage.Commit{}, fmt.Errorf("commit exceeds %d bytes", state.MaxCommitBytes)
	}
	// Admission: from here the store lifetime, not the caller, owns the work.
	opCtx, cancel := s.op()
	defer cancel()
	if err := s.persist(opCtx, c, revision, epoch); err != nil {
		if s.life.Err() != nil && !errors.Is(err, storage.ErrCommitUnknown) {
			// Close interrupted the transaction before COMMIT was sent.
			if t := s.terminal(); t != nil {
				return storage.Commit{}, t
			}
		}
		return storage.Commit{}, s.fail(err)
	}
	s.mu.Lock()
	s.revision = c.Revision
	if s.state == stateOpen {
		close(s.changed)
		s.changed = make(chan struct{})
	}
	s.mu.Unlock()
	return cloneCommit(c), nil
}

// persist writes c in one transaction on the lock-owning writer session.
// oldRevision and oldEpoch are the counters c must follow. Acquisition
// advances the epoch, every other commit keeps it.
func (s *Store) persist(ctx context.Context, c storage.Commit, oldRevision, oldEpoch uint64) (err error) {
	if c.Revision > math.MaxInt64 || c.Epoch > math.MaxInt64 || len(c.Operations) > math.MaxInt32 {
		return fmt.Errorf("continuous postgres counters exhausted")
	}
	tx, err := s.writer.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return safeError("begin commit", err)
	}
	committing := false
	defer func() {
		if !committing {
			rb, cancel := context.WithTimeout(context.Background(), s.opts.CloseTimeout)
			_ = tx.Rollback(rb)
			cancel()
		}
	}()
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		return safeError("commit", err)
	}
	var revision, epoch int64
	if err := tx.QueryRow(ctx, `SELECT revision, epoch
FROM zot_continuous.stores
WHERE id = $1
FOR UPDATE`, s.storeID).Scan(&revision, &epoch); err != nil {
		return safeError("lock store row", err)
	}
	if uint64(epoch) != oldEpoch {
		return fmt.Errorf("continuous postgres store %s: persisted epoch %d differs from owned epoch %d: %w", s.name, epoch, oldEpoch, storage.ErrFenced)
	}
	if uint64(revision) != oldRevision {
		return fmt.Errorf("continuous postgres store %s: persisted revision differs under exclusive ownership: %w", s.name, storage.ErrCorrupt)
	}
	batch := &pgx.Batch{}
	batch.Queue(`INSERT INTO zot_continuous.commits (store_id, revision, epoch, commit_schema, committed_time, actor, operation_count) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		s.storeID, int64(c.Revision), int64(c.Epoch), int32(c.Schema), c.Time.UTC().Format(time.RFC3339Nano), bytea(c.Actor), int32(len(c.Operations)))
	for i, op := range c.Operations {
		var value []byte
		if !op.Delete {
			value = append([]byte{}, op.Value...)
		}
		batch.Queue(`INSERT INTO zot_continuous.records (store_id, revision, ordinal, key, key_digest, key_prefix, deleted, value) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			s.storeID, int64(c.Revision), int32(i), bytea(op.Key), digest(op.Key), keyPrefix(op.Key), op.Delete, value)
	}
	batch.Queue(`UPDATE zot_continuous.stores
SET revision = $2, epoch = $3
WHERE id = $1 AND revision = $4 AND epoch = $5`, s.storeID, int64(c.Revision), int64(c.Epoch), int64(oldRevision), int64(oldEpoch))
	results := tx.SendBatch(ctx, batch)
	for i := 0; i < batch.Len(); i++ {
		tag, err := results.Exec()
		if err != nil {
			results.Close()
			return safeError("write commit", err)
		}
		if i == batch.Len()-1 && tag.RowsAffected() != 1 {
			results.Close()
			return fmt.Errorf("continuous postgres store %s: conditional counter update affected %d rows: %w", s.name, tag.RowsAffected(), storage.ErrCorrupt)
		}
	}
	if err := results.Close(); err != nil {
		return safeError("write commit", err)
	}
	committing = true
	return classifyCommit(tx.Commit(ctx))
}

// classifyCommit maps a COMMIT result. A definite rollback is an ordinary
// failure. Everything uncertain is ErrCommitUnknown: a lost reply can come
// from a durable commit even when the driver calls the error safe to retry.
func classifyCommit(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrTxCommitRollback):
		return safeError("commit", err, errRolledBack)
	case commitDefinitelyAborted(err):
		return safeError("commit", err, errRolledBack)
	}
	return safeError("commit", err, storage.ErrCommitUnknown)
}

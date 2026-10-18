// Package postgres provides a continuous store on an external PostgreSQL
// server, through the pure Go pgx driver. One Store instance owns one
// logical store: it holds a session advisory lock on a dedicated writer
// connection for its whole lifetime and reads through a second connection.
//
// Records are immutable versioned rows. A snapshot at revision R reads the
// newest version at or below R, so snapshots never copy records and opening
// never replays history. Every mutation is one READ COMMITTED transaction
// with synchronous_commit=on, checked against the persisted revision and
// writer epoch.
//
// The store never reconnects, retries a mutation, or takes over another
// writer. A lost session, an operation timeout, or a commit whose outcome is
// unknown terminates the instance: Done closes, Err reports the cause, and
// an explicit new Open is required. Caller contexts bound waiting for a
// connection, not database work already admitted, so a disconnecting client
// cannot destroy the writer session. See docs/continuous.md.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// MaxRawCommitBytes caps the raw actor, key, and value bytes of one commit.
// The serialized JSON limit alone does not bound them, because marshaling
// compacts raw JSON whitespace.
const MaxRawCommitBytes = 8 << 20

const maxKey = 4096
const prefixBytes = 512

type lifecycle int

const (
	stateOpen lifecycle = iota
	stateClosed
	statePoisoned
)

// Store is a PostgreSQL-backed continuous store. It implements
// storage.Store and storage.StoreLifecycle, and its snapshots implement
// storage.ContextSnapshot. Remote SDK code must use the checked reads.
type Store struct {
	opts    Options
	name    string
	storeID int32
	uuid    string

	writer *pgx.Conn
	reader *pgx.Conn
	wgate  chan struct{}
	rgate  chan struct{}

	// life is the store-owned operation lifetime, independent of callers.
	life   context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	state    lifecycle
	err      error
	revision uint64
	epoch    uint64
	changed  chan struct{}
	done     chan struct{}

	// Point reads are memoized by exact snapshot revision, under mu.
	records     map[recordCacheKey]json.RawMessage
	recordBytes int
	pages       map[pageCacheKey]cachedPage
	pageBytes   int
	pageItems   int
	recent      []cachedCommit
	recentBytes int

	heartbeat   sync.WaitGroup
	cleanupOnce sync.Once
	cleaned     chan struct{}
	cleanupErr  error
}

var (
	_ storage.Store           = (*Store)(nil)
	_ storage.StoreLifecycle  = (*Store)(nil)
	_ storage.ContextSnapshot = (*snapshot)(nil)
)

// Open connects to the configured primary, validates or creates the layout,
// acquires the writer lock of opts.StoreName, and commits a new writer
// epoch. The overall default timeout is 30 seconds, shortened by an earlier
// ctx deadline. Cancelling ctx after Open returns has no effect on the store.
//
// A creation-capable Open that fails may still have created the schema, the
// store row, or the acquisition commit. The next explicit Open inspects the
// actual persisted state.
func Open(ctx context.Context, dsn string, opts Options) (*Store, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	cfg, err := parseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if opts.dial != nil {
		cfg.DialFunc = opts.dial
	}
	if opts.lookup != nil {
		cfg.LookupFunc = opts.lookup
	}
	openCtx, cancelOpen := context.WithTimeout(ctx, defaultOpenTimeout)
	defer cancelOpen()
	life, cancel := context.WithCancel(context.Background())
	s := &Store{opts: opts, name: opts.StoreName, wgate: make(chan struct{}, 1), rgate: make(chan struct{}, 1),
		life: life, cancel: cancel, changed: make(chan struct{}), done: make(chan struct{}), cleaned: make(chan struct{})}
	ok := false
	defer func() {
		if !ok {
			s.abandon()
		}
	}()
	wrap := func(op string, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, storage.ErrLocked) && !errors.Is(err, ErrStoreNotFound) {
			return fmt.Errorf("continuous postgres %s: %w", op, ctxErr)
		}
		return err
	}
	if s.writer, err = pgx.ConnectConfig(openCtx, cfg); err != nil {
		return nil, wrap("connect", safeError("connect writer", err))
	}
	if err := checkServer(openCtx, s.writer, opts.TrustRemoteDurability); err != nil {
		return nil, wrap("connect", err)
	}
	var revision, epoch int64
	if revision, epoch, err = s.initialize(openCtx); err != nil {
		return nil, wrap("initialize", err)
	}
	var locked bool
	if err := s.writer.QueryRow(openCtx, `SELECT pg_try_advisory_lock($1::integer, $2::integer)`, writerNamespace, s.storeID).Scan(&locked); err != nil {
		return nil, wrap("lock", safeError("acquire writer lock", err))
	}
	if !locked {
		return nil, fmt.Errorf("continuous postgres store %s: %w", s.name, storage.ErrLocked)
	}
	if s.reader, err = pgx.ConnectConfig(openCtx, cfg); err != nil {
		return nil, wrap("connect", safeError("connect reader", err))
	}
	if err := checkServer(openCtx, s.reader, opts.TrustRemoteDurability); err != nil {
		return nil, wrap("connect", err)
	}
	var readerUUID string
	var readerID int32
	if err := s.reader.QueryRow(openCtx, `SELECT id, store_uuid::text FROM zot_continuous.stores WHERE name = $1`, s.name).Scan(&readerID, &readerUUID); err != nil {
		return nil, wrap("verify reader", safeError("verify reader", err))
	}
	if readerID != s.storeID || readerUUID != s.uuid {
		return nil, fmt.Errorf("continuous postgres reader and writer reach different stores, refusing a non-primary or mixed endpoint")
	}
	// Acquisition is a real commit: the new epoch is durable before any work.
	if epoch == math.MaxInt64 || revision == math.MaxInt64 {
		return nil, fmt.Errorf("continuous postgres store %s: counters exhausted", s.name)
	}
	acquire := storage.Commit{Schema: 1, Revision: uint64(revision) + 1, Epoch: uint64(epoch) + 1, Time: time.Now().UTC(), Actor: "writer.acquire"}
	if err := s.persist(openCtx, acquire, uint64(revision), uint64(epoch)); err != nil {
		return nil, wrap("acquire", err)
	}
	var seenRevision, seenEpoch int64
	if err := s.reader.QueryRow(openCtx, `SELECT revision, epoch FROM zot_continuous.stores WHERE id = $1`, s.storeID).Scan(&seenRevision, &seenEpoch); err != nil {
		return nil, wrap("verify reader", safeError("verify reader", err))
	}
	if uint64(seenRevision) != acquire.Revision || uint64(seenEpoch) != acquire.Epoch {
		return nil, fmt.Errorf("continuous postgres reader does not observe the acquisition commit, refusing a lagging endpoint")
	}
	s.revision, s.epoch = acquire.Revision, acquire.Epoch
	s.heartbeat.Add(1)
	go s.heartbeatLoop()
	ok = true
	return s, nil
}

// checkServer refuses unsupported versions, standbys, read-only sessions,
// and servers with fsync disabled. trustDurability skips only the fsync
// check, on explicit operator request.
func checkServer(ctx context.Context, conn *pgx.Conn, trustDurability bool) error {
	var version int
	var recovery bool
	var readOnly, fsync string
	err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::integer, pg_is_in_recovery(), current_setting('transaction_read_only'), current_setting('fsync')`).Scan(&version, &recovery, &readOnly, &fsync)
	if err != nil {
		return safeError("check server", err)
	}
	switch {
	case version < 150000 || version >= 190000:
		return fmt.Errorf("continuous postgres requires PostgreSQL 15 through 18 (server reports %d)", version)
	case recovery:
		return errors.New("continuous postgres requires a primary server, the endpoint is in recovery")
	case readOnly != "off":
		return errors.New("continuous postgres requires a read-write session")
	case fsync != "on" && !trustDurability:
		return errors.New("continuous postgres requires fsync=on (for a managed service with replicated remote storage such as Neon, see --postgres-trust-remote-durability)")
	}
	return nil
}

// initialize validates or creates the layout and resolves the store row in
// one transaction under the schema lock. It returns the persisted counters
// after checking them against the newest commit.
func (s *Store) initialize(ctx context.Context) (int64, int64, error) {
	tx, err := s.writer.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, 0, safeError("initialize", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		return 0, 0, safeError("initialize", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, schemaLockKey); err != nil {
		return 0, 0, safeError("lock schema", err)
	}
	exists, err := ensureLayout(ctx, tx, s.opts.Create)
	if err != nil {
		return 0, 0, err
	}
	if !exists {
		return 0, 0, fmt.Errorf("continuous postgres store %s: %w (schema %s absent)", s.name, ErrStoreNotFound, Schema)
	}
	var revision, epoch int64
	err = tx.QueryRow(ctx, `SELECT id, store_uuid::text, revision, epoch FROM zot_continuous.stores WHERE name = $1`, s.name).Scan(&s.storeID, &s.uuid, &revision, &epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		if !s.opts.Create {
			return 0, 0, fmt.Errorf("continuous postgres store %s: %w", s.name, ErrStoreNotFound)
		}
		s.uuid = uuid.NewString()
		err = tx.QueryRow(ctx, `INSERT INTO zot_continuous.stores (name, store_uuid) VALUES ($1, $2::uuid) RETURNING id`, s.name, s.uuid).Scan(&s.storeID)
	}
	if err != nil {
		return 0, 0, safeError("resolve store", err)
	}
	if s.storeID <= 0 {
		return 0, 0, corrupt("invalid store id")
	}
	if err := checkNewest(ctx, tx, s.storeID, revision, epoch); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, safeError("initialize", err)
	}
	return revision, epoch, nil
}

// checkNewest requires the newest commit to agree with the store counters.
// It reads one row and does not validate older history.
func checkNewest(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id int32, revision, epoch int64) error {
	if revision < 0 || epoch < 0 || (revision == 0) != (epoch == 0) || epoch > revision {
		return corrupt("store counters out of range")
	}
	var r, e int64
	var schema int32
	var ts string
	err := q.QueryRow(ctx, `SELECT revision, epoch, commit_schema, committed_time FROM zot_continuous.commits WHERE store_id = $1 ORDER BY revision DESC LIMIT 1`, id).Scan(&r, &e, &schema, &ts)
	if errors.Is(err, pgx.ErrNoRows) {
		if revision != 0 {
			return corrupt("store counters without commits")
		}
		return nil
	}
	if err != nil {
		return safeError("read newest commit", err)
	}
	if _, perr := time.Parse(time.RFC3339Nano, ts); r != revision || e != epoch || schema != 1 || perr != nil {
		return corrupt("newest commit does not match store counters")
	}
	return nil
}

// StoreUUID is the persistent identity of the logical store.
func (s *Store) StoreUUID() string { return s.uuid }

func (s *Store) Capabilities() storage.Capabilities {
	return storage.Capabilities{Durability: storage.Strict, Indexed: true, BoundedHistoryMemory: true}
}

func (s *Store) Epoch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

// Done closes once on normal closure or terminal failure.
func (s *Store) Done() <-chan struct{} { return s.done }

// Err is nil while healthy and after a normal Close, otherwise the first
// terminal cause, joined with storage.ErrCommitUnknown when a commit
// outcome became unknown while another failure was unwinding.
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// terminal returns the error for a call on a store that is not open.
func (s *Store) terminal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminalLocked()
}

func (s *Store) terminalLocked() error {
	switch s.state {
	case statePoisoned:
		return s.err
	case stateClosed:
		return storage.ErrClosed
	}
	return nil
}

// fail poisons the store with cause. The first cause is kept. An unknown
// commit outcome discovered later is joined, never dropped. Cleanup runs
// asynchronously so the caller may hold a connection gate.
func (s *Store) fail(cause error) error {
	s.mu.Lock()
	switch {
	case s.err == nil:
		s.err = cause
	case errors.Is(cause, storage.ErrCommitUnknown) && !errors.Is(s.err, storage.ErrCommitUnknown):
		s.err = errors.Join(s.err, cause)
	}
	if s.state != statePoisoned {
		if s.state == stateOpen {
			close(s.done)
			close(s.changed)
		}
		s.state = statePoisoned
	}
	err := s.err
	s.mu.Unlock()
	go s.cleanup()
	return err
}

// enter waits for a connection gate. Waiting observes the caller, store
// shutdown, and the admission limit. Failing here issues no database
// command and leaves the store healthy.
func (s *Store) enter(ctx context.Context, gate chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.terminal(); err != nil {
		return err
	}
	t := time.NewTimer(s.opts.admissionWait)
	defer t.Stop()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.terminal()
	case <-t.C:
		return fmt.Errorf("continuous postgres: timed out after %s waiting for the database connection", s.opts.admissionWait)
	}
	if err := ctx.Err(); err != nil {
		<-gate
		return err
	}
	if err := s.terminal(); err != nil {
		<-gate
		return err
	}
	return nil
}

// op derives an admitted operation context from the store lifetime, never
// from the caller.
func (s *Store) op() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.life, s.opts.OperationTimeout)
}

// Close marks the store closed, wakes waiters, cancels admitted work, stops
// the heartbeat, and closes both sessions with bounded cleanup. It returns
// the terminal cause of a poisoned store and any cleanup failure. It is
// safe to call repeatedly and concurrently.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.state == stateOpen {
		s.state = stateClosed
		close(s.done)
		close(s.changed)
	}
	s.mu.Unlock()
	s.cleanup()
	return errors.Join(s.Err(), s.cleanupErr)
}

// cleanup cancels the lifetime, joins the heartbeat, then closes each
// connection once its in-flight command has unwound. Gates stay held, so
// nothing runs on a closed connection.
func (s *Store) cleanup() {
	s.cleanupOnce.Do(func() {
		s.cancel()
		s.heartbeat.Wait()
		var errs []error
		for _, c := range []struct {
			conn *pgx.Conn
			gate chan struct{}
			name string
		}{{s.writer, s.wgate, "writer"}, {s.reader, s.rgate, "reader"}} {
			if c.conn == nil {
				continue
			}
			t := time.NewTimer(s.opts.CloseTimeout)
			select {
			case c.gate <- struct{}{}:
				t.Stop()
			case <-t.C:
				errs = append(errs, fmt.Errorf("continuous postgres close: %s operation did not unwind within %s", c.name, s.opts.CloseTimeout))
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.CloseTimeout)
			if err := c.conn.Close(ctx); err != nil {
				errs = append(errs, safeError("close "+c.name, err))
			}
			cancel()
		}
		s.cleanupErr = errors.Join(errs...)
		close(s.cleaned)
	})
	<-s.cleaned
}

// abandon releases a partially opened store.
func (s *Store) abandon() {
	s.mu.Lock()
	if s.state == stateOpen {
		s.state = stateClosed
		close(s.done)
		close(s.changed)
	}
	s.mu.Unlock()
	s.cleanup()
}

func (s *Store) heartbeatLoop() {
	defer s.heartbeat.Done()
	t := time.NewTimer(s.opts.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-s.life.Done():
			return
		case <-t.C:
		}
		// A busy writer delays the probe until its operation completes.
		select {
		case s.wgate <- struct{}{}:
		case <-s.life.Done():
			return
		}
		err := s.probe()
		<-s.wgate
		if err == nil {
			err = s.probeReader()
		}
		if err != nil {
			if s.terminal() == nil {
				s.fail(err)
			}
			return
		}
		t.Reset(s.opts.HeartbeatInterval)
	}
}

// probe verifies the writer session still holds the writer lock and that
// persisted counters match acknowledged state. It holds the writer gate.
func (s *Store) probe() error {
	ctx, cancel := context.WithTimeout(s.life, s.opts.HeartbeatTimeout)
	defer cancel()
	var revision, epoch int64
	var held bool
	err := s.writer.QueryRow(ctx, `SELECT s.revision, s.epoch, EXISTS (
    SELECT 1 FROM pg_catalog.pg_locks l
    WHERE l.locktype = 'advisory' AND l.pid = pg_backend_pid() AND l.granted
      AND l.database = (SELECT oid FROM pg_catalog.pg_database WHERE datname = current_database())
      AND l.classid = $2::oid AND l.objid = $3::oid AND l.objsubid = 2)
FROM zot_continuous.stores s WHERE s.id = $1`, s.storeID, uint32(writerNamespace), uint32(s.storeID)).Scan(&revision, &epoch, &held)
	if err != nil {
		if s.life.Err() != nil {
			return nil
		}
		return safeError("heartbeat", err)
	}
	s.mu.Lock()
	wantRevision, wantEpoch := s.revision, s.epoch
	s.mu.Unlock()
	switch {
	case !held:
		return fmt.Errorf("continuous postgres heartbeat: writer lock lost: %w", storage.ErrFenced)
	case uint64(epoch) != wantEpoch:
		return fmt.Errorf("continuous postgres heartbeat: persisted epoch changed: %w", storage.ErrFenced)
	case uint64(revision) != wantRevision:
		return fmt.Errorf("continuous postgres heartbeat: persisted revision changed: %w", storage.ErrCorrupt)
	}
	return nil
}

// probeReader checks the reader session when it is idle.
func (s *Store) probeReader() error {
	select {
	case s.rgate <- struct{}{}:
	default:
		return nil
	}
	defer func() { <-s.rgate }()
	ctx, cancel := context.WithTimeout(s.life, s.opts.HeartbeatTimeout)
	defer cancel()
	var one int
	if err := s.reader.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		if s.life.Err() != nil {
			return nil
		}
		return safeError("heartbeat reader", err)
	}
	return nil
}

func digest(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

func keyPrefix(key string) []byte {
	if len(key) > prefixBytes {
		key = key[:prefixBytes]
	}
	return []byte(key)
}

// validateOps mirrors the shared operation rules and the PostgreSQL raw
// payload guard.
func validateOps(actor string, ops []storage.Operation) error {
	seen := make(map[string]bool, len(ops))
	raw := len(actor)
	for _, op := range ops {
		if op.Key == "" || len(op.Key) > maxKey || seen[op.Key] {
			return fmt.Errorf("invalid or duplicate record key")
		}
		seen[op.Key] = true
		if op.Delete {
			if len(op.Value) != 0 {
				return fmt.Errorf("delete must not contain a value")
			}
		} else if !json.Valid(op.Value) {
			return fmt.Errorf("record value must be valid JSON")
		}
		raw += len(op.Key) + len(op.Value)
		if raw > MaxRawCommitBytes {
			return fmt.Errorf("commit exceeds %d raw payload bytes", MaxRawCommitBytes)
		}
	}
	if len(ops) > math.MaxInt32 {
		return fmt.Errorf("too many operations")
	}
	return nil
}

func cloneCommit(c storage.Commit) storage.Commit {
	c.Operations = append([]storage.Operation(nil), c.Operations...)
	for i := range c.Operations {
		if c.Operations[i].Value != nil {
			c.Operations[i].Value = append(json.RawMessage(nil), c.Operations[i].Value...)
		}
	}
	return c
}

package postgres

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/conformance"
)

// testDSN returns the dedicated disposable fixture. Only
// ZOT_TEST_POSTGRES_DSN is consulted, never production variables. CI sets
// ZOT_TEST_POSTGRES_REQUIRED so a missing fixture fails instead of skipping.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ZOT_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("ZOT_TEST_POSTGRES_REQUIRED") != "" {
			t.Fatal("ZOT_TEST_POSTGRES_REQUIRED is set but ZOT_TEST_POSTGRES_DSN is empty")
		}
		t.Skip("ZOT_TEST_POSTGRES_DSN not set, skipping PostgreSQL integration test")
	}
	return dsn
}

func uniqueName(t *testing.T) string {
	return "t-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
}

func openTest(t *testing.T, name string, mod ...func(*Options)) *Store {
	t.Helper()
	opts := Options{StoreName: name, Create: true}
	for _, m := range mod {
		m(&opts)
	}
	s, err := Open(context.Background(), testDSN(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// admin opens an independent session for observation and fault injection.
func admin(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func persisted(t *testing.T, conn *pgx.Conn, name string) (uint64, uint64) {
	t.Helper()
	var r, e int64
	if err := conn.QueryRow(context.Background(), `SELECT revision, epoch FROM zot_continuous.stores WHERE name = $1`, name).Scan(&r, &e); err != nil {
		t.Fatal(err)
	}
	return uint64(r), uint64(e)
}

func commit(t *testing.T, s *Store, ops ...storage.Operation) storage.Commit {
	t.Helper()
	snap, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Commit(context.Background(), storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: ops})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func put(k, v string) storage.Operation {
	return storage.Operation{Key: k, Value: json.RawMessage(v)}
}

func TestConformance(t *testing.T) {
	testDSN(t)
	conformance.Run(t, func(t *testing.T) storage.Store { return openTest(t, uniqueName(t)) })
}

// countWrites measures protocol flushes, not wall-clock latency. With small,
// warmed statements each flush is one sequential database round trip.
type countWrites struct {
	net.Conn
	writes atomic.Int64
}

func (c *countWrites) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

func TestCommitRoundTrips(t *testing.T) {
	var writer *countWrites
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		counted := &countWrites{Conn: conn}
		if writer == nil {
			writer = counted
		}
		return counted, nil
	}
	s, err := Open(context.Background(), testDSN(t), Options{
		StoreName: uniqueName(t), Create: true, dial: dial, HeartbeatInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if s.writer.Config().TLSConfig != nil {
		t.Skip("protocol flush counting requires a plaintext fixture")
	}
	// Warm statement descriptions before counting steady-state flushes.
	commit(t, s, put("k", `1`))
	before := writer.writes.Load()
	commit(t, s, put("k", `2`))
	if got := writer.writes.Load() - before; got != 3 {
		t.Fatalf("commit used %d protocol flushes, want 3", got)
	}
}

func TestAcquisitionAndReopen(t *testing.T) {
	name := uniqueName(t)
	a := openTest(t, name)
	if a.Epoch() != 1 {
		t.Fatalf("first epoch %d", a.Epoch())
	}
	commit(t, a, put("k", `1`))
	id := a.StoreUUID()
	if _, err := Open(context.Background(), testDSN(t), Options{StoreName: name}); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("second writer: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := openTest(t, name, func(o *Options) { o.Create = false })
	if b.Epoch() != 2 || b.StoreUUID() != id {
		t.Fatalf("reopen epoch %d uuid %s", b.Epoch(), b.StoreUUID())
	}
	snap, _ := b.Snapshot(context.Background())
	if snap.Revision() != 3 {
		t.Fatalf("revision %d", snap.Revision())
	}
	commits, err := b.Scan(context.Background(), 0, 10)
	if err != nil || len(commits) != 3 || commits[0].Actor != "writer.acquire" || commits[2].Actor != "writer.acquire" || commits[2].Epoch != 2 || len(commits[2].Operations) != 0 {
		t.Fatalf("history: %+v %v", commits, err)
	}
}

func TestExistingOnlyLeavesAbsentAuthorityAbsent(t *testing.T) {
	name := uniqueName(t)
	if _, err := Open(context.Background(), testDSN(t), Options{StoreName: name}); !errors.Is(err, ErrStoreNotFound) {
		t.Fatalf("existing-only open: %v", err)
	}
	var n int
	if err := admin(t).QueryRow(context.Background(), `SELECT count(*) FROM zot_continuous.stores WHERE name = $1`, name).Scan(&n); err != nil && !strings.Contains(err.Error(), "does not exist") {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("existing-only open created a store")
	}
}

func TestIndependentStores(t *testing.T) {
	a := openTest(t, uniqueName(t))
	b := openTest(t, uniqueName(t))
	commit(t, a, put("k", `"a"`))
	commit(t, b, put("k", `"b"`))
	for s, want := range map[*Store]string{a: `"a"`, b: `"b"`} {
		snap, _ := s.Snapshot(context.Background())
		v, ok, err := storage.Get(context.Background(), snap, "k")
		if err != nil || !ok || string(v) != want {
			t.Fatalf("store isolation: %s %v %v", v, ok, err)
		}
	}
}

func TestRepresentation(t *testing.T) {
	s := openTest(t, uniqueName(t))
	ctx := context.Background()
	long := strings.Repeat("\xff", 4096)
	keys := []string{"nul\x00key", "ü/ñ", "\xff\xfe raw", long, "p/" + strings.Repeat("x", 600) + "/1", "p/" + strings.Repeat("x", 600) + "/2"}
	var ops []storage.Operation
	for i, k := range keys {
		ops = append(ops, put(k, fmt.Sprintf(`{ "b" : 1,  "a": %d.50 }`, i)))
	}
	ops = append(ops, put("jnull", `null`))
	c := commit(t, s, ops...)
	snap, _ := s.Snapshot(ctx)
	for i, k := range keys {
		v, ok, err := storage.Get(ctx, snap, k)
		if err != nil || !ok || string(v) != fmt.Sprintf(`{ "b" : 1,  "a": %d.50 }`, i) {
			t.Fatalf("key %q: %s %v %v", k[:min(len(k), 10)], v, ok, err)
		}
	}
	if v, ok, err := storage.Get(ctx, snap, "jnull"); err != nil || !ok || string(v) != "null" {
		t.Fatalf("json null: %s %v %v", v, ok, err)
	}
	commit(t, s, storage.Operation{Key: "jnull", Delete: true})
	later, _ := s.Snapshot(ctx)
	if _, ok, _ := storage.Get(ctx, later, "jnull"); ok {
		t.Fatal("tombstone visible")
	}
	if _, ok, _ := storage.Get(ctx, snap, "jnull"); !ok {
		t.Fatal("old snapshot changed")
	}
	// Long prefix beyond the 512-byte index bound uses exact matching.
	page, err := storage.Page(ctx, snap, "p/"+strings.Repeat("x", 600)+"/", "", 10)
	if err != nil || len(page) != 2 {
		t.Fatalf("long prefix page: %d %v", len(page), err)
	}
	page, err = storage.Page(ctx, snap, "\xff", "", 10)
	if err != nil || len(page) != 2 {
		t.Fatalf("0xff prefix: %d %v", len(page), err)
	}
	page, err = storage.Page(ctx, snap, "nul\x00", "", 10)
	if err != nil || len(page) != 1 {
		t.Fatalf("nul prefix: %d %v", len(page), err)
	}
	// A prefix equal to a key matches that key.
	page, err = storage.Page(ctx, snap, "ü/ñ", "", 10)
	if err != nil || len(page) != 1 {
		t.Fatalf("prefix equal to key: %d %v", len(page), err)
	}
	// Scan returns the raw spelling and copies.
	commits, err := s.Scan(ctx, c.Revision-1, 1)
	if err != nil || string(commits[0].Operations[0].Value) != `{ "b" : 1,  "a": 0.50 }` {
		t.Fatalf("scan raw json: %v", err)
	}
	commits[0].Operations[0].Value[0] = 'x'
	again, _ := s.Scan(ctx, c.Revision-1, 1)
	if again[0].Operations[0].Value[0] != '{' {
		t.Fatal("scan aliases")
	}
	// Zero-operation commits are valid and appear in history.
	z := commit(t, s)
	commits, err = s.Scan(ctx, z.Revision-1, 1)
	if err != nil || len(commits) != 1 || len(commits[0].Operations) != 0 {
		t.Fatalf("empty commit: %+v %v", commits, err)
	}
}

func TestDigestCollisionDistinguished(t *testing.T) {
	s := openTest(t, uniqueName(t))
	commit(t, s, put("a", `1`))
	conn := admin(t)
	// Force a colliding digest for a second key by writing a raw row in
	// the store's next revision, the way a collision would be stored.
	ctx := context.Background()
	snap, _ := s.Snapshot(ctx)
	rev := int64(snap.Revision() + 1)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO zot_continuous.commits VALUES ($1, $2, $3, 1, $4, ''::bytea, 1)`, s.storeID, rev, int64(s.Epoch()), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO zot_continuous.records VALUES ($1, $2, 0, 'b'::bytea, $3, 'b'::bytea, false, '2'::bytea)`, s.storeID, rev, digest("a")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	view := &snapshot{s: s, revision: uint64(rev)}
	if v, ok, err := view.GetContext(ctx, "a"); err != nil || !ok || string(v) != "1" {
		t.Fatalf("collision misread: %s %v %v", v, ok, err)
	}
	if _, ok, err := view.GetContext(ctx, "b"); err != nil || ok {
		t.Fatalf("lookup by digest without key: %v %v", ok, err)
	}
}

// TestReferenceModel compares snapshots and pagination with an in-memory
// model across many revisions, deletes, and cursors.
func TestReferenceModel(t *testing.T) {
	s := openTest(t, uniqueName(t))
	ctx := context.Background()
	type version struct {
		rev   uint64
		model map[string]string
	}
	model := map[string]string{}
	var history []version
	keys := []string{"a", "a/1", "a/2", "a/\xff", "b", "b\x00", "ab", "a/1/x", "\xff", "\xff\xff", "c/" + strings.Repeat("z", 520), "c/" + strings.Repeat("z", 520) + "y"}
	seed := uint64(7)
	next := func() uint64 { seed = seed*6364136223846793005 + 1442695040888963407; return seed >> 33 }
	for r := 0; r < 60; r++ {
		var ops []storage.Operation
		used := map[string]bool{}
		for i := 0; i < 1+int(next()%4); i++ {
			k := keys[next()%uint64(len(keys))]
			if used[k] {
				continue
			}
			used[k] = true
			if next()%3 == 0 {
				ops = append(ops, storage.Operation{Key: k, Delete: true})
				delete(model, k)
			} else {
				v := fmt.Sprint(next() % 1000)
				ops = append(ops, put(k, v))
				model[k] = v
			}
		}
		c := commit(t, s, ops...)
		cp := map[string]string{}
		for k, v := range model {
			cp[k] = v
		}
		history = append(history, version{c.Revision, cp})
	}
	for _, h := range history {
		view := &snapshot{s: s, revision: h.rev}
		for _, prefix := range []string{"", "a", "a/", "a/1", "b", "\xff", "c/" + strings.Repeat("z", 520), "zz"} {
			var want []string
			for k := range h.model {
				if strings.HasPrefix(k, prefix) {
					want = append(want, k)
				}
			}
			sort.Strings(want)
			var got []string
			after := ""
			for {
				page, err := view.PageContext(ctx, prefix, after, 2)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range page {
					if string(r.Value) != h.model[r.Key] {
						t.Fatalf("rev %d key %q value %s want %s", h.rev, r.Key, r.Value, h.model[r.Key])
					}
					got = append(got, r.Key)
				}
				if len(page) < 2 {
					break
				}
				after = page[len(page)-1].Key
			}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("rev %d prefix %q: got %q want %q", h.rev, prefix, got, want)
			}
		}
		for _, k := range keys {
			v, ok, err := view.GetContext(ctx, k)
			want, has := h.model[k]
			if err != nil || ok != has || (ok && string(v) != want) {
				t.Fatalf("rev %d get %q: %s %v %v", h.rev, k, v, ok, err)
			}
		}
	}
}

func TestPayloadLimits(t *testing.T) {
	s := openTest(t, uniqueName(t))
	ctx := context.Background()
	snap, _ := s.Snapshot(ctx)
	// Whitespace compacts in JSON marshaling, but raw bytes count.
	big := json.RawMessage("[" + strings.Repeat(" ", MaxRawCommitBytes) + "1]")
	if _, err := s.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{{Key: "k", Value: big}}}); err == nil {
		t.Fatal("raw payload limit not enforced")
	}
	if s.Err() != nil {
		t.Fatal("validation failure poisoned the store")
	}
	key := strings.Repeat("k", 4097)
	if _, err := s.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{{Key: key, Value: json.RawMessage(`1`)}}}); err == nil {
		t.Fatal("long key accepted")
	}
}

func TestCallerCancellationKeepsSessionsHealthy(t *testing.T) {
	s := openTest(t, uniqueName(t))
	ctx := context.Background()
	commit(t, s, put("k", `1`))
	// Reopen with a cold cache so these reads exercise database admission,
	// not the acknowledged-write cache.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, s.name, func(o *Options) { o.admissionWait = 200 * time.Millisecond })
	// Hold the reader gate so the caller waits at admission.
	s.rgate <- struct{}{}
	snap, _ := s.Snapshot(ctx)
	waitCtx, cancel := context.WithCancel(ctx)
	errs := make(chan error, 1)
	go func() {
		_, _, err := storage.Get(waitCtx, snap, "k")
		errs <- err
	}()
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("gate wait: %v", err)
	}
	// Admission timeout is not a store failure either.
	if _, _, err := storage.Get(ctx, snap, "k"); err == nil || !strings.Contains(err.Error(), "waiting for the database connection") {
		t.Fatalf("admission timeout: %v", err)
	}
	<-s.rgate
	if s.Err() != nil {
		t.Fatalf("store poisoned by caller: %v", s.Err())
	}
	// Admitted reads finish. A caller that gave up afterwards sees its error
	// while the connection stays usable.
	if v, ok, err := storage.Get(ctx, snap, "k"); err != nil || !ok || string(v) != "1" {
		t.Fatalf("read after cancellation: %s %v %v", v, ok, err)
	}
	commit(t, s, put("k", `2`))
}

func TestAdmittedCommitSurvivesCallerCancellation(t *testing.T) {
	name := uniqueName(t)
	s := openTest(t, name)
	conn := admin(t)
	ctx := context.Background()
	// Block the commit on the store row lock held by another session.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM zot_continuous.stores WHERE name = $1 FOR UPDATE`, name); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot(ctx)
	callCtx, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() {
		_, err := s.Commit(callCtx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{put("k", `1`)}})
		result <- err
	}()
	waitFor(t, func() bool {
		var n int
		admin(t).QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name = 'zot-continuous' AND wait_event_type = 'Lock'`).Scan(&n)
		return n > 0
	})
	cancel()
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatalf("admitted commit lost to caller cancellation: %v", err)
	}
	if r, _ := persisted(t, conn, name); r != snap.Revision()+1 {
		t.Fatalf("persisted revision %d", r)
	}
	if s.Err() != nil {
		t.Fatal(s.Err())
	}
}

func TestOperationDeadlinePoisonsAndWakes(t *testing.T) {
	name := uniqueName(t)
	s := openTest(t, name, func(o *Options) { o.OperationTimeout = 300 * time.Millisecond })
	conn := admin(t)
	ctx := context.Background()
	tx, _ := conn.Begin(ctx)
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM zot_continuous.stores WHERE name = $1 FOR UPDATE`, name); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot(ctx)
	waited := make(chan error, 1)
	go func() { waited <- s.Wait(ctx, snap.Revision()) }()
	_, err := s.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{put("k", `1`)}})
	if err == nil || errors.Is(err, storage.ErrCommitUnknown) {
		t.Fatalf("deadline before COMMIT: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed")
	}
	if err := <-waited; err == nil {
		t.Fatal("waiter not woken")
	}
	if _, err := s.Snapshot(ctx); err == nil {
		t.Fatal("poisoned store still serves snapshots")
	}
	if s.Err() == nil {
		t.Fatal("no terminal cause")
	}
}

func TestHeartbeatDetectsTermination(t *testing.T) {
	s := openTest(t, uniqueName(t), func(o *Options) { o.HeartbeatInterval = 50 * time.Millisecond })
	conn := admin(t)
	var pid uint32 = s.writer.PgConn().PID()
	if _, err := conn.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, int32(pid)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("heartbeat did not detect session loss")
	}
	if s.Err() == nil {
		t.Fatal("no cause")
	}
	// No hidden reconnect: the instance stays dead and a fresh Open acquires
	// a new epoch from durable state, now that the lock is released.
	if _, err := s.Snapshot(context.Background()); err == nil {
		t.Fatal("store revived")
	}
	waitFor(t, func() bool {
		b, err := Open(context.Background(), testDSN(t), Options{StoreName: s.name})
		if err != nil {
			return false
		}
		defer b.Close()
		return b.Epoch() == s.Epoch()+1
	})
}

// TestFailureBeforeCommitPoisons makes the header insert fail inside the
// transaction. Nothing is persisted, the outcome is definite, and the store
// still terminates because ownership invariants were violated.
func TestFailureBeforeCommitPoisons(t *testing.T) {
	name := uniqueName(t)
	s := openTest(t, name)
	conn := admin(t)
	ctx := context.Background()
	before, _ := persisted(t, conn, name)
	snap, _ := s.Snapshot(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO zot_continuous.commits VALUES ($1, $2, $3, 1, $4, ''::bytea, 0)`, s.storeID, int64(snap.Revision()+1), int64(s.Epoch()), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{put("k", `1`)}})
	if err == nil || errors.Is(err, storage.ErrCommitUnknown) {
		t.Fatalf("failed insert: %v", err)
	}
	if strings.Contains(err.Error(), "duplicate key value") {
		t.Fatalf("server message leaked: %v", err)
	}
	if s.Err() == nil {
		t.Fatal("not poisoned")
	}
	if after, _ := persisted(t, conn, name); after != before {
		t.Fatal("partial commit persisted")
	}
	var n int
	conn.QueryRow(ctx, `SELECT count(*) FROM zot_continuous.records WHERE store_id = $1 AND revision = $2`, s.storeID, int64(snap.Revision()+1)).Scan(&n)
	if n != 0 {
		t.Fatal("operation rows persisted")
	}
}

func TestFencedByPersistedEpoch(t *testing.T) {
	name := uniqueName(t)
	s := openTest(t, name)
	conn := admin(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `UPDATE zot_continuous.stores SET epoch = epoch + 1, revision = revision + 1 WHERE name = $1`, name); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot(ctx)
	_, err := s.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{put("must-not-persist", `1`)}})
	if !errors.Is(err, storage.ErrFenced) || !errors.Is(s.Err(), storage.ErrFenced) {
		t.Fatalf("fencing: %v / %v", err, s.Err())
	}
	// The lock check and writes share one batch, but a fenced batch must
	// roll back both its header and its record rows before any COMMIT.
	var records, headers int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM zot_continuous.records WHERE store_id = $1 AND key = $2`, s.storeID, bytea("must-not-persist")).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM zot_continuous.commits WHERE store_id = $1 AND revision = $2`, s.storeID, int64(snap.Revision()+1)).Scan(&headers); err != nil {
		t.Fatal(err)
	}
	if records != 0 || headers != 0 {
		t.Fatal("fenced batch persisted partial writes")
	}
}

func TestLayoutRefusal(t *testing.T) {
	testDSN(t)
	conn := admin(t)
	ctx := context.Background()
	// Ensure the layout exists, then pretend a newer build wrote it.
	openTest(t, uniqueName(t)).Close()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, schemaLockKey); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE zot_continuous.schema_meta SET version = 2`); err != nil {
		t.Fatal(err)
	}
	// Validate inside the same transaction so other tests never observe it.
	if err := validateLayout(ctx, tx); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("newer layout: %v", err)
	}
}

// TestLostCommitAcknowledgement drops the COMMIT CommandComplete frame after
// the server committed. The store must report an unknown outcome, never
// retry, and a fresh open must see the durable revision.
func TestLostCommitAcknowledgement(t *testing.T) {
	name := uniqueName(t)
	var armed atomic.Bool
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &dropCommit{Conn: c, armed: &armed}, nil
	}
	s, err := Open(context.Background(), testDSN(t), Options{StoreName: name, Create: true, dial: dial})
	if err != nil {
		t.Skipf("frame interception requires a plaintext test fixture: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	snap, _ := s.Snapshot(ctx)
	armed.Store(true)
	_, err = s.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{put("k", `1`)}})
	if !errors.Is(err, storage.ErrCommitUnknown) {
		t.Fatalf("lost ack: %v", err)
	}
	if !errors.Is(s.Err(), storage.ErrCommitUnknown) {
		t.Fatalf("lifecycle cause: %v", s.Err())
	}
	if r, _ := persisted(t, admin(t), name); r != snap.Revision()+1 {
		t.Fatalf("expected the server to have committed, persisted revision %d", r)
	}
	s.Close()
	waitFor(t, func() bool {
		b, err := Open(ctx, testDSN(t), Options{StoreName: name})
		if err != nil {
			return false
		}
		defer b.Close()
		v, _ := b.Snapshot(ctx)
		got, ok, err := storage.Get(ctx, v, "k")
		return err == nil && ok && string(got) == "1"
	})
}

// dropCommit replaces the backend's CommandComplete "COMMIT" frame with a
// transport error once armed. Other frames pass through unchanged.
type dropCommit struct {
	net.Conn
	armed *atomic.Bool
	mu    sync.Mutex
	buf   []byte
	out   []byte
	dead  bool
}

func (d *dropCommit) Read(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.out) == 0 {
		if d.dead {
			return 0, io.ErrUnexpectedEOF
		}
		chunk := make([]byte, 32<<10)
		n, err := d.Conn.Read(chunk)
		d.buf = append(d.buf, chunk[:n]...)
		for len(d.buf) >= 5 {
			size := int(binary.BigEndian.Uint32(d.buf[1:5]))
			if len(d.buf) < 1+size {
				break
			}
			frame := d.buf[:1+size]
			if d.armed.Load() && frame[0] == 'C' && bytes.Equal(frame[5:], []byte("COMMIT\x00")) {
				d.dead = true
				d.Conn.Close()
				break
			}
			d.out = append(d.out, frame...)
			d.buf = d.buf[1+size:]
		}
		if d.dead && len(d.out) == 0 {
			return 0, io.ErrUnexpectedEOF
		}
		if err != nil && len(d.out) == 0 {
			return 0, err
		}
	}
	n := copy(p, d.out)
	d.out = d.out[n:]
	return n, nil
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached before deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestFsyncCheck runs against ZOT_TEST_POSTGRES_NOFSYNC_DSN, a disposable
// server started with fsync=off. Open refuses it unless the operator opts
// in with TrustRemoteDurability. Required integration runs must provide this
// fixture as well as the ordinary server.
func TestFsyncCheck(t *testing.T) {
	dsn := os.Getenv("ZOT_TEST_POSTGRES_NOFSYNC_DSN")
	if dsn == "" {
		if os.Getenv("ZOT_TEST_POSTGRES_REQUIRED") != "" {
			t.Fatal("ZOT_TEST_POSTGRES_REQUIRED is set but ZOT_TEST_POSTGRES_NOFSYNC_DSN is empty")
		}
		t.Skip("ZOT_TEST_POSTGRES_NOFSYNC_DSN not set")
	}
	name := uniqueName(t)
	_, err := Open(context.Background(), dsn, Options{StoreName: name, Create: true})
	if err == nil || !strings.Contains(err.Error(), "fsync=on") {
		t.Fatalf("fsync=off server accepted: %v", err)
	}
	s, err := Open(context.Background(), dsn, Options{StoreName: name, Create: true, TrustRemoteDurability: true})
	if err != nil {
		t.Fatalf("trusted open: %v", err)
	}
	defer s.Close()
	commit(t, s, put("k", `1`))
}

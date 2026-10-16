// Package storage defines the experimental continuous storage contract.
// Callers perform external work outside commits. Values are opaque JSON records.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrClosed   = errors.New("continuous store closed")
	ErrConflict = errors.New("continuous revision conflict")
	ErrFenced   = errors.New("continuous writer fenced")
	ErrLocked   = errors.New("continuous store already has a writer")
	ErrCorrupt  = errors.New("continuous journal corrupt")
	ErrCursor   = errors.New("continuous cursor outside retained history")
	// ErrCommitUnknown reports that a commit was submitted but its outcome
	// could not be determined. It may or may not be durable. The store is
	// closed and must be explicitly reopened to learn the actual state.
	ErrCommitUnknown = errors.New("continuous commit outcome unknown")
)

type Durability string

const (
	Memory  Durability = "memory"
	Process Durability = "process"
	Strict  Durability = "strict"
)

// Capabilities are explicit rather than inferred from the backend name.
type Capabilities struct {
	Durability           Durability `json:"durability"`
	Indexed              bool       `json:"indexed"`
	BoundedHistoryMemory bool       `json:"bounded_history_memory"`
}

type Record struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type Operation struct {
	Key    string          `json:"key"`
	Value  json.RawMessage `json:"value,omitempty"`
	Delete bool            `json:"delete,omitempty"`
}

type Commit struct {
	Schema     int         `json:"schema"`
	Revision   uint64      `json:"revision"`
	Epoch      uint64      `json:"epoch"`
	Time       time.Time   `json:"time"`
	Actor      string      `json:"actor,omitempty"`
	Operations []Operation `json:"operations"`
}

// Mutation uses a mandatory expected revision and writer epoch. A successful
// acknowledgement means the entire commit passed the backend persistence barrier.
type Mutation struct {
	ExpectedRevision uint64
	Epoch            uint64
	Actor            string
	Operations       []Operation
}

// Snapshot is detached from the store. Records and history are paginated
// separately so clients do not need an unbounded subscription buffer.
type Snapshot interface {
	Revision() uint64
	Get(key string) (json.RawMessage, bool)
	Page(prefix, after string, limit int) ([]Record, error)
}

// ContextSnapshot is a Snapshot whose reads can fail, for example because
// they reach a remote database. Remote backends implement it, and callers
// must use Get and Page (or these methods) so a read error is never
// mistaken for an absent record.
type ContextSnapshot interface {
	Snapshot
	GetContext(context.Context, string) (json.RawMessage, bool, error)
	PageContext(context.Context, string, string, int) ([]Record, error)
}

// Get reads key through the checked method when the snapshot provides one.
// Local snapshots get an initial context check and their ordinary read.
func Get(ctx context.Context, snap Snapshot, key string) (json.RawMessage, bool, error) {
	if c, ok := snap.(ContextSnapshot); ok {
		return c.GetContext(ctx, key)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	b, ok := snap.Get(key)
	return b, ok, nil
}

// Page lists records through the checked method when available.
func Page(ctx context.Context, snap Snapshot, prefix, after string, limit int) ([]Record, error) {
	if c, ok := snap.(ContextSnapshot); ok {
		return c.PageContext(ctx, prefix, after, limit)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return snap.Page(prefix, after, limit)
}

// StoreLifecycle is implemented by stores that can fail independently of a
// call, such as a database session that is lost. Done closes once on normal
// closure or terminal failure. Err is nil while healthy and after a normal
// Close, and otherwise reports the first terminal cause.
type StoreLifecycle interface {
	Done() <-chan struct{}
	Err() error
}

// Store has one writer per open instance. Scan plus Wait provides a gap-free
// pull subscription: Wait returns immediately if a newer revision already exists.
// Cancelling Wait never cancels admitted work. Close unblocks every waiter.
// This initial contract is provisional, not a stable public SDK.
type Store interface {
	Capabilities() Capabilities
	Epoch() uint64
	Snapshot(context.Context) (Snapshot, error)
	Commit(context.Context, Mutation) (Commit, error)
	Scan(context.Context, uint64, int) ([]Commit, error)
	Wait(context.Context, uint64) error
	Close() error
}

package postgres

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// ErrStoreNotFound reports that an existing-only open found no schema or no
// store with the requested name. Nothing was created.
var ErrStoreNotFound = errors.New("continuous postgres store not found")

var sqlstatePattern = regexp.MustCompile(`^[0-9A-Z]{5}$`)

// dbError is a redacted database failure. It carries an operation label,
// an optional SQLSTATE, and zot sentinels for errors.Is, never driver
// messages, DETAIL or CONTEXT fields, DSNs, or record contents.
type dbError struct {
	op     string
	detail string
	code   string
	kinds  []error
}

func (e *dbError) Error() string {
	msg := "continuous postgres " + e.op
	if e.detail != "" {
		msg += ": " + e.detail
	}
	if e.code != "" {
		msg += " (SQLSTATE " + e.code + ")"
	}
	for _, k := range e.kinds {
		msg += ": " + k.Error()
	}
	return msg
}

func (e *dbError) Unwrap() []error { return e.kinds }

// safeError redacts err. Context errors of the caller are returned by the
// callers directly. Here every context error comes from the store's own
// operation lifetime and is reported as a timeout or shutdown.
func safeError(op string, err error, kinds ...error) error {
	if err == nil {
		return nil
	}
	e := &dbError{op: op, kinds: kinds}
	for _, sentinel := range []error{storage.ErrClosed, storage.ErrCorrupt, storage.ErrFenced, storage.ErrLocked, storage.ErrCommitUnknown, ErrStoreNotFound} {
		if errors.Is(err, sentinel) && !containsErr(e.kinds, sentinel) {
			e.kinds = append(e.kinds, sentinel)
		}
	}
	var pg *pgconn.PgError
	var verify *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostname x509.HostnameError
	switch {
	case errors.As(err, &pg):
		if sqlstatePattern.MatchString(pg.Code) {
			e.code = pg.Code
		}
		e.detail = "server error"
	case errors.As(err, &verify), errors.As(err, &unknownCA), errors.As(err, &hostname):
		e.detail = "server certificate verification failed"
	case errors.Is(err, context.DeadlineExceeded), pgconn.Timeout(err):
		e.detail = "timed out"
	case errors.Is(err, context.Canceled):
		e.detail = "interrupted by store shutdown"
	case len(e.kinds) > 0:
	default:
		e.detail = "connection failed"
	}
	return e
}

func containsErr(list []error, target error) bool {
	for _, e := range list {
		if e == target {
			return true
		}
	}
	return false
}

func sqlstate(err error) (code, severity string, ok bool) {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return "", "", false
	}
	severity = pg.SeverityUnlocalized
	if severity == "" {
		severity = pg.Severity
	}
	return pg.Code, severity, true
}

// commitDefinitelyAborted reports whether a COMMIT error is an ordinary
// ERROR that PostgreSQL guarantees aborted the transaction: integrity
// violations, serialization failure, deadlock, or an already failed
// transaction. Everything else, including FATAL errors and lost replies,
// is an unknown outcome.
func commitDefinitelyAborted(err error) bool {
	code, severity, ok := sqlstate(err)
	if !ok || severity != "ERROR" {
		return false
	}
	switch {
	case len(code) == 5 && code[:2] == "23":
		return true
	case code == "40001", code == "40P01", code == "25P02":
		return true
	}
	return false
}

var errRolledBack = errors.New("commit rolled back")

func corrupt(what string) error {
	return fmt.Errorf("%w: %s", storage.ErrCorrupt, what)
}

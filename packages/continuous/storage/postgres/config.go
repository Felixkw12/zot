package postgres

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// MaxDSNBytes bounds a connection string from any source.
const MaxDSNBytes = 64 << 10

// maxMessageBody bounds a single PostgreSQL protocol message. It leaves room
// for an 8 MiB commit in bytea text encoding without allowing unbounded
// server messages.
const maxMessageBody = 32 << 20

const (
	defaultOperationTimeout  = 15 * time.Second
	defaultHeartbeatInterval = 5 * time.Second
	defaultHeartbeatTimeout  = 5 * time.Second
	defaultCloseTimeout      = 5 * time.Second
	defaultOpenTimeout       = 30 * time.Second
	defaultAdmissionWait     = 15 * time.Second
)

// allowedKeys is the exact connection-string allowlist. Environment
// variables, passfiles, and service files remain trusted operator inputs.
// The effective result is validated after parsing.
var allowedKeys = []string{
	"host", "port", "database", "dbname", "user", "password", "passfile",
	"service", "servicefile", "connect_timeout", "sslmode", "sslrootcert",
	"sslcert", "sslkey", "sslpassword", "sslsni", "sslnegotiation",
	"target_session_attrs", "channel_binding", "require_auth",
}

var storeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidStoreName reports whether name is a valid logical store name.
func ValidStoreName(name string) bool { return storeNamePattern.MatchString(name) }

// Options configure Open. Zero durations select the documented defaults.
type Options struct {
	// StoreName is the case-sensitive logical store, required.
	StoreName string
	// Create allows creating the schema and the named store when absent.
	// Without it, a missing schema or store returns ErrStoreNotFound.
	Create bool
	// Durability must be empty or strict.
	Durability storage.Durability
	// OperationTimeout bounds every admitted database operation. Exceeding
	// it terminates the store. Default 15s.
	OperationTimeout time.Duration
	// HeartbeatInterval is the pause between ownership probes. Default 5s.
	HeartbeatInterval time.Duration
	// HeartbeatTimeout bounds one probe. Default 5s.
	HeartbeatTimeout time.Duration
	// CloseTimeout bounds each cleanup step of Close. Default 5s.
	CloseTimeout time.Duration
	// TrustRemoteDurability skips only the fsync=on requirement, for
	// services such as Neon whose durability comes from replicated remote
	// storage rather than local fsync. zot cannot verify that claim: it
	// trusts the operator and the provider. Every other server check stays.
	TrustRemoteDurability bool

	// admissionWait bounds waiting for a connection gate. Tests shorten it.
	admissionWait time.Duration
	// dial replaces the network dialer in tests that intercept frames.
	dial pgconn.DialFunc
	// lookup replaces name resolution in tests that route a hostname.
	lookup pgconn.LookupFunc
}

func (o *Options) normalize() error {
	if !ValidStoreName(o.StoreName) {
		return fmt.Errorf("postgres store name must match [A-Za-z0-9][A-Za-z0-9._-]* and be at most 128 characters")
	}
	if o.Durability == "" {
		o.Durability = storage.Strict
	}
	if o.Durability != storage.Strict {
		return fmt.Errorf("postgres backend supports only strict durability")
	}
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{
		{&o.OperationTimeout, defaultOperationTimeout},
		{&o.HeartbeatInterval, defaultHeartbeatInterval},
		{&o.HeartbeatTimeout, defaultHeartbeatTimeout},
		{&o.CloseTimeout, defaultCloseTimeout},
		{&o.admissionWait, defaultAdmissionWait},
	} {
		if *d.v < 0 {
			return fmt.Errorf("postgres option durations must not be negative")
		}
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	return nil
}

// ValidateDSNText applies the source-independent content checks: size,
// UTF-8, no NUL, and a single line. It never echoes the content.
func ValidateDSNText(dsn string) error {
	switch {
	case dsn == "":
		return errors.New("postgres connection string is empty")
	case len(dsn) > MaxDSNBytes:
		return fmt.Errorf("postgres connection string exceeds %d bytes", MaxDSNBytes)
	case !utf8.ValidString(dsn):
		return errors.New("postgres connection string is not valid UTF-8")
	case strings.ContainsRune(dsn, 0):
		return errors.New("postgres connection string contains NUL")
	case strings.ContainsAny(dsn, "\r\n"):
		return errors.New("postgres connection string must be a single line")
	}
	return nil
}

// parseConfig parses dsn with the pinned driver and enforces the transport
// policy on the effective configuration. Errors never include the DSN.
func parseConfig(dsn string) (*pgx.ConnConfig, error) {
	if err := ValidateDSNText(dsn); err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfigWithOptions(dsn, pgx.ParseConfigOptions{ParseConfigOptions: pgconn.ParseConfigOptions{ConnStringAllowedKeys: allowedKeys}})
	if err != nil {
		// The driver error may quote parts of the connection string.
		return nil, errors.New("invalid postgres connection string or unsupported parameter (allowed: " + strings.Join(allowedKeys, ", ") + ")")
	}
	if err := checkTransport(&cfg.Config); err != nil {
		return nil, err
	}
	if cfg.TLSConfig != nil {
		tc := cfg.TLSConfig.Clone()
		if tc.MinVersion < tls.VersionTLS12 {
			tc.MinVersion = tls.VersionTLS12
		}
		cfg.TLSConfig = tc
	}
	cfg.RuntimeParams = map[string]string{
		"application_name":   "zot-continuous",
		"client_encoding":    "UTF8",
		"search_path":        "pg_catalog",
		"synchronous_commit": "on",
	}
	// Primary, read-write, and fsync checks run explicitly after connect,
	// overriding any parsed target_session_attrs preference.
	cfg.ValidateConnect = nil
	cfg.AfterNetConnect = nil
	cfg.OnNotice = nil
	cfg.OnNotification = nil
	cfg.Tracer = nil
	cfg.MaxProtocolMessageBodyLen = maxMessageBody
	return cfg, nil
}

// checkTransport accepts one endpoint: a Unix socket on Unix, fully
// verified TLS, or plaintext to a numeric loopback address.
func checkTransport(cfg *pgconn.Config) error {
	if len(cfg.Fallbacks) != 0 {
		return errors.New("postgres connection must name one host with sslmode=verify-full, or sslmode=disable for a numeric loopback address or Unix socket (multi-host and sslmode prefer/allow fallbacks are not supported)")
	}
	network, _ := pgconn.NetworkAddress(cfg.Host, cfg.Port)
	if network == "unix" {
		if runtime.GOOS == "windows" {
			return errors.New("postgres Unix-domain sockets are not supported on Windows")
		}
		return nil
	}
	if cfg.TLSConfig == nil {
		ip := net.ParseIP(cfg.Host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("plaintext postgres connections are allowed only to a numeric loopback address such as 127.0.0.1 or ::1, otherwise use sslmode=verify-full")
		}
		return nil
	}
	tc := cfg.TLSConfig
	if tc.InsecureSkipVerify || tc.ServerName == "" || tc.VerifyPeerCertificate != nil || tc.VerifyConnection != nil {
		return errors.New("postgres TLS must verify the server certificate and hostname (sslmode=verify-full)")
	}
	return nil
}

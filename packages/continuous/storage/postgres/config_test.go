package postgres

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// clearPGEnv isolates parsing from the developer's PG* environment.
func clearPGEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGPASSFILE", "PGSERVICE", "PGSERVICEFILE", "PGSSLMODE", "PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY", "PGSSLSNI", "PGTARGETSESSIONATTRS", "PGCONNECT_TIMEOUT", "PGOPTIONS", "PGAPPNAME", "PGSSLNEGOTIATION", "PGCHANNELBINDING", "PGREQUIREAUTH"} {
		t.Setenv(k, "")
	}
	t.Setenv("PGSERVICEFILE", t.TempDir()+"/none")
	t.Setenv("PGPASSFILE", t.TempDir()+"/none")
}

func TestTransportPolicy(t *testing.T) {
	clearPGEnv(t)
	cases := []struct {
		dsn string
		ok  bool
	}{
		{"postgres://u:p@127.0.0.1:5432/db?sslmode=disable", true},
		{"postgres://u:p@[::1]:5432/db?sslmode=disable", true},
		{"host=127.0.0.1 user=u sslmode=disable", true},
		{"postgres://u:p@db.example.com/db?sslmode=verify-full", true},
		{"postgres://u:p@localhost/db?sslmode=disable", false},
		{"postgres://u:p@10.0.0.5/db?sslmode=disable", false},
		{"postgres://u:p@db.example.com/db?sslmode=require", false},
		{"postgres://u:p@db.example.com/db?sslmode=verify-ca", false},
		{"postgres://u:p@db.example.com/db?sslmode=prefer", false},
		{"postgres://u:p@db.example.com/db?sslmode=allow", false},
		{"postgres://u:p@db.example.com/db", false},
		{"postgres://u:p@127.0.0.1/db", false},
		{"postgres://u:p@a.example.com,b.example.com/db?sslmode=verify-full", false},
		{"postgres://u:p@127.0.0.1/db?sslmode=disable&options=-c%20search_path%3Devil", false},
		{"postgres://u:p@127.0.0.1/db?sslmode=disable&application_name=x", false},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, struct {
			dsn string
			ok  bool
		}{"host=/var/run/postgresql user=u sslmode=disable", true})
	}
	for _, c := range cases {
		_, err := parseConfig(c.dsn)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.dsn, err, c.ok)
		}
		if err != nil && strings.Contains(err.Error(), ":p@") {
			t.Errorf("error leaks the DSN: %v", err)
		}
	}
}

func TestNeonPoolerUsesDirectEndpoint(t *testing.T) {
	clearPGEnv(t)
	const direct = "ep-example.us-east-2.aws.neon.tech"
	for _, dsn := range []string{
		"postgres://u:private-password@ep-example-pooler.us-east-2.aws.neon.tech:5433/db?sslmode=verify-full",
		"host=EP-EXAMPLE-POOLER.US-EAST-2.AWS.NEON.TECH. port=5433 user=u password=private-password dbname=db sslmode=verify-full",
	} {
		cfg, err := parseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Host != direct || cfg.TLSConfig.ServerName != direct {
			t.Fatalf("host=%q TLS server name=%q, want direct endpoint", cfg.Host, cfg.TLSConfig.ServerName)
		}
		if cfg.User != "u" || cfg.Password != "private-password" || cfg.Database != "db" || cfg.Port != 5433 {
			t.Fatal("endpoint routing changed other connection fields")
		}
		if cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.MinVersion < 0x0303 || len(cfg.Fallbacks) != 0 {
			t.Fatal("endpoint routing weakened transport policy")
		}
	}
	for _, host := range []string{
		direct,
		"session-pooler.example.com",
		"ep-example-pooler.neon.tech.example.com",
		"not-an-endpoint-pooler.us-east-2.aws.neon.tech",
	} {
		cfg, err := parseConfig("host=" + host + " user=u sslmode=verify-full")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Host != host || cfg.TLSConfig.ServerName != host {
			t.Fatal("non-Neon-pooler endpoint was rewritten")
		}
	}
	t.Setenv("PGHOST", "ep-example-pooler.us-east-2.aws.neon.tech")
	cfg, err := parseConfig("user=u sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != direct || cfg.TLSConfig.ServerName != direct {
		t.Fatal("pooler from environment was not routed to direct endpoint")
	}
}

func TestEnvironmentFallbacksRejected(t *testing.T) {
	clearPGEnv(t)
	// No sslmode in the DSN: the environment default "prefer" produces a
	// plaintext fallback, which is refused for remote hosts.
	t.Setenv("PGSSLMODE", "prefer")
	if _, err := parseConfig("host=db.example.com user=u"); err == nil {
		t.Fatal("prefer fallback accepted")
	}
	t.Setenv("PGSSLMODE", "verify-full")
	if _, err := parseConfig("host=db.example.com user=u"); err != nil {
		t.Fatalf("verified environment TLS rejected: %v", err)
	}
}

func TestEffectiveConfigOverrides(t *testing.T) {
	clearPGEnv(t)
	t.Setenv("PGOPTIONS", "-c search_path=evil")
	t.Setenv("PGTARGETSESSIONATTRS", "any")
	cfg, err := parseConfig("postgres://u:p@db.example.com/db?sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"application_name": "zot-continuous", "client_encoding": "UTF8", "search_path": "pg_catalog", "synchronous_commit": "on"}
	if fmt.Sprint(cfg.RuntimeParams) != fmt.Sprint(want) {
		t.Fatalf("runtime params: %v", cfg.RuntimeParams)
	}
	if cfg.TLSConfig.MinVersion < 0x0303 || cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.ServerName != "db.example.com" {
		t.Fatalf("tls: %+v", cfg.TLSConfig)
	}
	if cfg.MaxProtocolMessageBodyLen != 32<<20 || cfg.ValidateConnect != nil || cfg.AfterNetConnect != nil || len(cfg.Fallbacks) != 0 {
		t.Fatal("effective config not hardened")
	}
}

func TestDSNTextChecks(t *testing.T) {
	for _, bad := range []string{"", "host=127.0.0.1\nuser=u", "host=127.0.0.1\x00", string([]byte{0xff, 0xfe}), strings.Repeat("a", MaxDSNBytes+1)} {
		if err := ValidateDSNText(bad); err == nil {
			t.Errorf("accepted %q", bad[:min(len(bad), 20)])
		}
	}
	if err := ValidateDSNText("host=127.0.0.1"); err != nil {
		t.Fatal(err)
	}
}

func TestOptions(t *testing.T) {
	o := Options{StoreName: "main"}
	if err := o.normalize(); err != nil || o.OperationTimeout != 15*time.Second || o.HeartbeatInterval != 5*time.Second || o.Durability != storage.Strict {
		t.Fatalf("defaults: %+v %v", o, err)
	}
	for _, bad := range []Options{{}, {StoreName: ".x"}, {StoreName: strings.Repeat("a", 129)}, {StoreName: "a b"}, {StoreName: "x", Durability: storage.Process}, {StoreName: "x", OperationTimeout: -1}} {
		if err := bad.normalize(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if !ValidStoreName(strings.Repeat("a", 128)) || !ValidStoreName("A.b-c_9") {
		t.Fatal("valid names rejected")
	}
}

func TestPrefixUpper(t *testing.T) {
	cases := map[string]string{"a": "b", "a\xff": "b", "\xff\xff": "", "ab\x00": "ab\x01", "": ""}
	for in, want := range cases {
		got := prefixUpper([]byte(in))
		if string(got) != want || (want == "" && got != nil) {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestCommitClassification(t *testing.T) {
	pg := func(code, severity string) error {
		return &pgconn.PgError{Code: code, Severity: severity, SeverityUnlocalized: severity, Message: "secret detail", Detail: "row contents"}
	}
	for _, c := range []struct {
		err     error
		unknown bool
	}{
		{pg("23503", "ERROR"), false},
		{pg("40001", "ERROR"), false},
		{pg("40P01", "ERROR"), false},
		{pg("25P02", "ERROR"), false},
		{pg("40003", "ERROR"), true},
		{pg("57P01", "FATAL"), true},
		{pg("23505", "FATAL"), true},
		{errors.New("conn closed"), true},
	} {
		err := classifyCommit(c.err)
		if errors.Is(err, storage.ErrCommitUnknown) != c.unknown {
			t.Errorf("%v: %v", c.err, err)
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "row contents") {
			t.Errorf("leaks server text: %v", err)
		}
	}
}

func TestRawPayloadGuard(t *testing.T) {
	big := `"` + strings.Repeat(" ", 0) + strings.Repeat("a", MaxRawCommitBytes) + `"`
	if err := validateOps("", []storage.Operation{{Key: "k", Value: []byte(big)}}); err == nil {
		t.Fatal("raw payload above limit accepted")
	}
	if err := validateOps("", []storage.Operation{{Key: "k", Value: []byte(`1`)}, {Key: "k", Value: []byte(`2`)}}); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if err := validateOps("", []storage.Operation{{Key: "k", Delete: true, Value: []byte(`1`)}}); err == nil {
		t.Fatal("delete with value accepted")
	}
}

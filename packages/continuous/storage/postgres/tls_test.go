package postgres

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
)

// TestTLSTransport runs real PostgreSQL traffic over verified TLS. It needs
// ZOT_TEST_POSTGRES_TLS_ADDR (host:port of a TLS-enabled fixture),
// ZOT_TEST_POSTGRES_TLS_CA (its CA), ZOT_TEST_POSTGRES_TLS_OTHER_CA (an
// unrelated CA), and a server certificate for pg.zot.test. Connections to
// that name are routed to the fixture address, so certificate and hostname
// verification run exactly as in production.
func TestTLSTransport(t *testing.T) {
	addr, ca, other := os.Getenv("ZOT_TEST_POSTGRES_TLS_ADDR"), os.Getenv("ZOT_TEST_POSTGRES_TLS_CA"), os.Getenv("ZOT_TEST_POSTGRES_TLS_OTHER_CA")
	if addr == "" || ca == "" || other == "" {
		if os.Getenv("ZOT_TEST_POSTGRES_TLS_REQUIRED") != "" {
			t.Fatal("TLS fixture required but not configured")
		}
		t.Skip("TLS fixture not configured")
	}
	clearPGEnv(t)
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	lookup := func(context.Context, string) ([]string, error) { return []string{"192.0.2.1"}, nil }
	open := func(host, root string) error {
		dsn := "postgres://postgres:zottest@" + host + ":5432/zottest?sslmode=verify-full&sslrootcert=" + root
		s, err := Open(context.Background(), dsn, Options{StoreName: uniqueName(t), Create: true, dial: dial, lookup: lookup})
		if err == nil {
			defer s.Close()
			commit(t, s, put("k", `1`))
		}
		return err
	}
	if err := open("pg.zot.test", ca); err != nil {
		t.Fatalf("verified TLS: %v", err)
	}
	for _, c := range []struct{ host, root string }{{"wrong.zot.test", ca}, {"pg.zot.test", other}} {
		err := open(c.host, c.root)
		if err == nil || !strings.Contains(err.Error(), "certificate verification failed") {
			t.Fatalf("%s with %s: %v", c.host, c.root, err)
		}
		if strings.Contains(err.Error(), "zottest@") {
			t.Fatalf("leaks DSN: %v", err)
		}
	}
	// The fixture still accepts plaintext, but a non-loopback plaintext
	// configuration is refused before connecting.
	if _, err := Open(context.Background(), "postgres://postgres:zottest@pg.zot.test/zottest?sslmode=disable", Options{StoreName: "x", dial: dial}); err == nil {
		t.Fatal("remote plaintext accepted")
	}
}

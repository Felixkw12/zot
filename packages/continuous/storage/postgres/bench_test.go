package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Benchmarks need ZOT_TEST_POSTGRES_DSN. They measure this implementation on
// whatever fixture is configured; numbers depend on the server and network.

func lookupDSN() string { return os.Getenv("ZOT_TEST_POSTGRES_DSN") }

func benchStore(b *testing.B) *Store {
	b.Helper()
	dsn := lookupDSN()
	if dsn == "" {
		b.Skip("ZOT_TEST_POSTGRES_DSN not set")
	}
	name := "b-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	s, err := Open(context.Background(), dsn, Options{StoreName: name, Create: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

// fill writes revisions commits of width keys each, cycling over keys.
func fill(b *testing.B, s *Store, revisions, width, keys int, prefix string) {
	ctx := context.Background()
	for r := 0; r < revisions; r++ {
		ops := make([]storage.Operation, 0, width)
		for i := 0; i < width; i++ {
			ops = append(ops, storage.Operation{Key: fmt.Sprintf("%s%05d", prefix, (r*width+i)%keys), Value: json.RawMessage(fmt.Sprintf(`{"r":%d}`, r))})
		}
		snap, _ := s.Snapshot(ctx)
		if _, err := s.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: ops}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCommit(b *testing.B) {
	for _, width := range []int{1, 50} {
		b.Run(fmt.Sprint("ops=", width), func(b *testing.B) {
			s := benchStore(b)
			b.ReportAllocs()
			b.ResetTimer()
			fill(b, s, b.N, width, 1<<30, "k/")
		})
	}
}

func BenchmarkHotKeyGet(b *testing.B) {
	s := benchStore(b)
	fill(b, s, 5000, 1, 1, "hot/")
	ctx := context.Background()
	snap, _ := s.Snapshot(ctx)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, err := storage.Get(ctx, snap, "hot/00000"); err != nil || !ok {
			b.Fatal(err)
		}
	}
}

// BenchmarkPageTraversal reads every live key of a prefix in pages of 50,
// over 50,000 versions of 1,000 keys, the shape of the design probe.
func BenchmarkPageTraversal(b *testing.B) {
	s := benchStore(b)
	fill(b, s, 500, 100, 1000, "wide/")
	fill(b, s, 50, 2, 20, "narrow/")
	ctx := context.Background()
	snap, _ := s.Snapshot(ctx)
	for _, prefix := range []string{"wide/", "narrow/"} {
		b.Run(strings.TrimSuffix(prefix, "/"), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				after, n := "", 0
				for {
					page, err := storage.Page(ctx, snap, prefix, after, 50)
					if err != nil {
						b.Fatal(err)
					}
					n += len(page)
					if len(page) < 50 {
						break
					}
					after = page[len(page)-1].Key
				}
				if n == 0 {
					b.Fatal("empty traversal")
				}
			}
		})
	}
}

func BenchmarkScan(b *testing.B) {
	s := benchStore(b)
	fill(b, s, 1000, 10, 1<<30, "s/")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if c, err := s.Scan(ctx, 0, 1000); err != nil || len(c) != 1000 {
			b.Fatal(err)
		}
	}
}

// BenchmarkOpen measures reopening over a long history. Open reads only the
// newest commit, so its cost and allocations must not grow with history.
func BenchmarkOpen(b *testing.B) {
	for _, history := range []int{10, 5000} {
		b.Run(fmt.Sprint("history=", history), func(b *testing.B) {
			s := benchStore(b)
			fill(b, s, history, 1, 1<<30, "o/")
			name := s.name
			s.Close()
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				o, err := Open(ctx, lookupDSN(), Options{StoreName: name})
				if err != nil {
					b.Fatal(err)
				}
				o.Close()
			}
		})
	}
}

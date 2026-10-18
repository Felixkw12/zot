package postgres

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type immediateClient struct{}

func (immediateClient) Name() string { return "synthetic" }
func (immediateClient) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 2)
	ch <- provider.EventTextDelta{Delta: "hello"}
	ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "hello"}}}}
	close(ch)
	return ch, nil
}

// BenchmarkPromptRoundTrip exercises the real task executor with an immediate
// synthetic provider. Protocol flush counts expose remote-latency costs that
// a loopback timing benchmark would hide. No provider API is called.
func BenchmarkPromptRoundTrip(b *testing.B) {
	dsn := lookupDSN()
	if dsn == "" {
		b.Skip("ZOT_TEST_POSTGRES_DSN not set")
	}
	var sessions []*countWrites
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		counted := &countWrites{Conn: conn}
		sessions = append(sessions, counted)
		return counted, nil
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn, Options{StoreName: "bench-" + uuid.NewString(), Create: true, dial: dial, HeartbeatInterval: time.Hour})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	if s.writer.Config().TLSConfig != nil || len(sessions) != 2 {
		b.Skip("protocol flush counting requires a plaintext fixture with two sessions")
	}
	r, err := continuous.New(s)
	if err != nil {
		b.Fatal(err)
	}
	engine := continuous.EngineFunc(func(context.Context, continuous.Conversation) (*core.Agent, error) {
		return core.NewAgent(immediateClient{}, "synthetic", "", core.NewRegistry()), nil
	})
	svc, err := continuous.NewService(r, engine, continuous.ExecutionOptions{PartialFlushInterval: -1})
	if err != nil {
		b.Fatal(err)
	}
	c, err := r.OpenRoot(ctx, "bench", continuous.AgentConfig{Model: "synthetic"})
	if err != nil {
		b.Fatal(err)
	}
	step := func() {
		if _, err := r.Submit(ctx, c.ID, "bench", "", "hello"); err != nil {
			b.Fatal(err)
		}
		if run, _, err := svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
			b.Fatalf("synthetic prompt did not settle: %v", err)
		}
	}
	step() // Warm statement descriptions and initial runtime reads.
	start, err := s.Snapshot(ctx)
	if err != nil {
		b.Fatal(err)
	}
	writes, reads := sessions[0].writes.Load(), sessions[1].writes.Load()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		step()
	}
	b.StopTimer()
	end, err := s.Snapshot(ctx)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(end.Revision()-start.Revision())/float64(b.N), "commits/op")
	b.ReportMetric(float64(sessions[0].writes.Load()-writes)/float64(b.N), "writer-flushes/op")
	b.ReportMetric(float64(sessions[1].writes.Load()-reads)/float64(b.N), "reader-flushes/op")
}

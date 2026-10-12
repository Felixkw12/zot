package continuous

import (
	"context"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
)

func TestCompactionForOverridesHostPolicy(t *testing.T) {
	ctx := context.Background()
	h, client := compactionHarness(t, []scriptStep{{text: long(200)}, {text: long(200)}, {text: "small"}})
	defer h.r.Close()
	c := h.root(t)
	h.svc.opts.Compaction = CompactionPolicy{ContextWindow: 100000}
	seen := 0
	h.svc.opts.CompactionFor = func(config AgentConfig) CompactionPolicy {
		if config.Provider != c.Config.Provider || config.Model != c.Config.Model {
			t.Error("compaction callback did not receive the conversation selection")
		}
		seen++
		return CompactionPolicy{ContextWindow: 400, ReserveTokens: 50, KeepRecentTokens: 150}
	}
	for _, prompt := range []string{long(200), long(200), "third"} {
		if _, err := h.r.Submit(ctx, c.ID, "actor", "", prompt); err != nil {
			t.Fatal(err)
		}
		if run, _, err := h.svc.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
			t.Fatalf("step: %+v %v", run, err)
		}
	}
	if seen < 3 || client.summaries == 0 {
		t.Fatalf("selected policy not used: callbacks=%d summaries=%d", seen, client.summaries)
	}
	last := h.client.requests[len(h.client.requests)-1]
	if !strings.Contains(core.MessageText(last.Messages[0]), "SUMMARY") {
		t.Fatal("missing compacted context")
	}
}

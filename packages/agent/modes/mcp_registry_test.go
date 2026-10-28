package modes

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
)

func TestBtwPreservesTurnGuard(t *testing.T) {
	client := &btwToolTestClient{done: make(chan struct{})}
	main := core.NewAgent(client, "test", "", core.Registry{"echo": &btwEchoTool{}})
	calls := 0
	main.BeforeTurnContext = func(ctx context.Context, step int) (bool, string) {
		calls++
		return false, "denied by extension"
	}
	side := newBtwAgent(main, "", "test")
	_ = side.Prompt(context.Background(), "hello", nil, nil)
	client.mu.Lock()
	requests := len(client.requests)
	client.mu.Unlock()
	if calls != 1 || requests != 0 {
		t.Fatalf("turn guard calls=%d, provider requests=%d, want 1 and 0", calls, requests)
	}
}

func TestConcurrentCatalogRefreshAndRegistryReaders(t *testing.T) {
	main := core.NewAgent(nil, "test", "", core.Registry{})
	i := &Interactive{agent: main}
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Go(func() {
		<-start
		for n := range 100 {
			main.UpdateTools(func(current core.Registry) core.Registry {
				current[fmt.Sprintf("mcp__test__%d", n)] = &btwEchoTool{}
				return current
			})
		}
	})
	close(start)
	for range 1000 {
		_ = newBtwAgent(main, "", "test")
		_ = i.powerShellSettingHint()
		i.applyAutoSwarmTool(false)
		i.applyOpenRouterServerTools(false)
		i.applyPowerShellSetting(false, "windows")
	}
	wg.Wait()
	if got := len(main.ToolRegistry()); got != 100 {
		t.Fatalf("settings toggle lost catalog updates: got %d tools, want 100", got)
	}
}

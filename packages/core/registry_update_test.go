package core

import (
	"fmt"
	"sync"
	"testing"
)

func TestUpdateToolsPreservesConcurrentChanges(t *testing.T) {
	for _, initial := range []Registry{nil, {}} {
		a := NewAgent(nil, "test", "", initial)
		var wg sync.WaitGroup
		for n := range 20 {
			wg.Go(func() {
				a.UpdateTools(func(current Registry) Registry {
					current[fmt.Sprint(n)] = &outcomeTool{}
					return current
				})
			})
		}
		wg.Wait()
		if got := len(a.ToolRegistry()); got != 20 {
			t.Fatalf("concurrent update lost tools: got %d", got)
		}
		if len(initial) != 0 {
			t.Fatal("update mutated an older registry snapshot")
		}
	}
}

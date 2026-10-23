package main

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// hub.admin.rebind reads the agents map under h.mu: a join or a spawn writes it concurrently. Run
// with -race; without the lock the race detector (or the map's own check) fails the test.
func TestAdminRebindReadsAgentsUnderLock(t *testing.T) {
	h, _ := workerTestHub(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.mu.Lock()
			h.agents["agent-2"] = &Agent{ID: "agent-2"}
			delete(h.agents, "agent-2")
			h.mu.Unlock()
		}
	}()
	for i := 0; i < 1000; i++ {
		_, err := h.adminAction(context.Background(), "hub.admin.rebind", map[string]any{"agent": "unknown", "block_id": "block:1"})
		if err == nil || !strings.Contains(err.Error(), "rebind needs a roster agent") {
			close(stop)
			wg.Wait()
			t.Fatalf("rebind of an unknown agent: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

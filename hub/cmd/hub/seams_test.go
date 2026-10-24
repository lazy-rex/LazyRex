package main

import "context"

// Test seams: small wrappers that only tests call. They live here so that the hub binary has no
// unreachable code (deadcode).

func (h *Hub) peerAfter(ctx context.Context, peer string) (int64, error) {
	after, _, err := h.peerCursor(ctx, peer)
	return after, err
}

func (h *Hub) resolveOnce(ctx context.Context) error {
	_, err := h.resolveNext(ctx)
	return err
}

// workerIdentity is the identity field of a worker: for an open spawn with an agent_id (the spawn
// row, not the current profile, which can change or go), its pane's identity; "" (no field) for
// every other worker.
func (h *Hub) workerIdentity(ctx context.Context, s *spawnRow) (string, error) {
	state, _, err := h.workerPane(ctx, s)
	return state, err
}

// request is loadRequest with no notice (the tests of the cache).
func (l *pluginLoads) request(c *loadCache, key string, refresh bool) (loadReply, error) {
	return l.requestNotify(c, key, refresh, nil, func() {})
}

// wait returns when every load goroutine has ended, its notice included. Tests call it after they
// cancel the run context, so no notice outlives the test.
func (l *pluginLoads) wait() { l.goroutines.Wait() }

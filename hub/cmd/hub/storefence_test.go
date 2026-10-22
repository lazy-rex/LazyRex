package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fenceTestHub has a plugin p whose registered instance is client c1.
func fenceTestHub(t *testing.T) (*Hub, *Plugin) {
	h, _ := workerTestHub(t)
	pl := &Plugin{Name: "p", ClientID: "c1", InstanceID: "i1"}
	h.plugins = map[string]*Plugin{"p": pl}
	h.vs = &viewSvc{dirty: map[string]bool{}, workers: map[string]chan struct{}{}}
	h.faults = &faults{}
	return h, pl
}

func batchAs(h *Hub, client, key string) error {
	p := perform{Action: "hub.store.batch", Args: json.RawMessage(`{"ops":[{"key":"` + key + `","value":1}]}`)}
	p.Requester.ClientID = client
	_, err := h.dispatch(context.Background(), p)
	return err
}

func records(h *Hub, key string) int {
	var n int
	h.db.QueryRow(`SELECT count(*) FROM records WHERE ns = 'p' AND key = ?`, key).Scan(&n)
	return n
}

// holdBatch starts a batch of c1 with the key and holds it at the stage of storeBatchHook. It
// returns the release function and the batch result.
func holdBatch(t *testing.T, h *Hub, stage, key string) (func(), chan error) {
	held, release := make(chan struct{}), make(chan struct{})
	storeBatchHook = func(s string) {
		if s == stage {
			close(held)
			<-release
		}
	}
	t.Cleanup(func() { storeBatchHook = nil })
	done := make(chan error, 1)
	go func() { done <- batchAs(h, "c1", key) }()
	<-held
	return func() { close(release) }, done
}

// retireClient is the client part of a retirement: the client ID goes, then the fence. It returns
// when the fence returns.
func retireClient(h *Hub, pl *Plugin) chan struct{} {
	h.mu.Lock()
	pl.ClientID = ""
	h.mu.Unlock()
	fenced := make(chan struct{})
	go func() {
		h.fenceStoreWrites(pl)
		close(fenced)
	}()
	return fenced
}

// A store write that passed the caller check before a retirement, and did not reach the fence,
// does not commit after it: it checks its caller again under the fence.
func TestStoreWriteFence(t *testing.T) {
	h, pl := fenceTestHub(t)
	release, done := holdBatch(t, h, "check", "late")
	fenced := retireClient(h, pl)
	<-fenced // the write holds no fence yet
	release()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "stale_instance") {
		t.Fatalf("the held write after the retirement: %v", err)
	}
	if records(h, "late") != 0 {
		t.Fatal("the held write committed after the retirement")
	}
	if err := batchAs(h, "c1", "after"); err == nil {
		t.Fatal("a write of the old client passed")
	}
}

// A store write that holds the fence makes the retirement wait: the write commits before the fence
// returns, never after it.
func TestStoreWriteFenceWaits(t *testing.T) {
	h, pl := fenceTestHub(t)
	release, done := holdBatch(t, h, "fenced", "early")
	fenced := retireClient(h, pl)
	select {
	case <-fenced:
		t.Fatal("the fence returned while a write held it")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("the write that held the fence: %v", err)
	}
	<-fenced
	if records(h, "early") != 1 {
		t.Fatal("the write that held the fence did not commit")
	}
}

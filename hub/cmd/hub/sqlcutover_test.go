package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// cutoverHub is fenceTestHub with a notice in the manifest and a roster agent.
func cutoverHub(t *testing.T) (*Hub, *Plugin) {
	h, pl := fenceTestHub(t)
	pl.run = pluginRun{Notice: "n"}
	h.agents["claude:a"] = &Agent{ID: "claude:a", delivered: map[string]int64{}}
	return h, pl
}

func notifyAs(h *Hub, client string) error {
	p := perform{Action: "hub.store.batch", Args: json.RawMessage(`{"ops":[{"notify":{"agent":"claude:a","pending":true}}]}`)}
	p.Requester.ClientID = client
	_, err := h.dispatch(context.Background(), p)
	return err
}

func holdNotify(t *testing.T, h *Hub, stage string) (func(), chan error) {
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	storeBatchHook = func(s string) {
		if s == stage {
			once.Do(func() { close(held); <-release }) // only the first notify waits
		}
	}
	t.Cleanup(func() { storeBatchHook = nil })
	done := make(chan error, 1)
	go func() { done <- notifyAs(h, "c1") }()
	<-held
	return func() { close(release) }, done
}

func noticeRows(h *Hub) int {
	var n int
	h.db.QueryRow(`SELECT count(*) FROM notices WHERE ns = 'p'`).Scan(&n)
	return n
}

// A store notify that passed dispatch before the notice path moved to hub.sql does not commit
// after the move: it checks notices_moved under the store fence. Before, the check ran before the
// fence, so the store notice could land after the SQL notices.
func TestSQLCutoverChecksUnderTheFence(t *testing.T) {
	h, _ := cutoverHub(t)
	release, done := holdNotify(t, h, "check")
	if err := h.db.moveNotices(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "notices_moved") {
		t.Fatalf("the held notify after the move: %v", err)
	}
	if noticeRows(h) != 0 {
		t.Fatal("the held notify committed after the move")
	}
}

// The move waits for a store notify that holds the fence, so that notify commits before any SQL
// notice. Before, the move did not wait.
func TestSQLCutoverWaitsForAStoreNotify(t *testing.T) {
	h, _ := cutoverHub(t)
	release, done := holdNotify(t, h, "fenced")
	moved := make(chan error, 1)
	go func() { moved <- h.admitSQLNotices("p", []noticeOp{{agent: "claude:a", pending: true}}) }()
	select {
	case err := <-moved:
		t.Fatalf("the move did not wait for the store notify: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("the store notify before the move: %v", err)
	}
	if err := <-moved; err != nil {
		t.Fatal(err)
	}
	if noticeRows(h) != 1 {
		t.Fatal("the store notify did not commit")
	}
	if err := notifyAs(h, "c1"); err == nil || !strings.Contains(err.Error(), "notices_moved") {
		t.Fatalf("a store notify after the move: %v", err)
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

// fakePeerServer is the peer's Rex server for peerCall: its lazyrex-hub clients by ID with their
// connection state, as `rex client ls --json` and `rex -C <target> do` see them.
type fakePeerServer struct {
	mu      sync.Mutex
	clients map[string]string // client ID -> "connected" or "lost"
	listOK  bool
	lists   int
	targets []string // the -C target of each do, in order
}

func (f *fakePeerServer) set(clients map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clients = clients
}

func (f *fakePeerServer) run(_ context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := slices.Index(argv, "ls"); i > 0 && argv[i-1] == "client" {
		f.lists++
		if !f.listOK {
			return nil, errors.New("exit status 1: Error: dial: connection refused")
		}
		var list []map[string]any
		for id, state := range f.clients {
			list = append(list, map[string]any{"client_id": id, "info": map[string]any{"label": "lazyrex-hub"}, "connection_state": state})
		}
		list = append(list, map[string]any{"client_id": "client:app", "info": map[string]any{"label": "Mac"}, "connection_state": "connected"})
		return json.Marshal(map[string]any{"clients": list})
	}
	i := slices.Index(argv, "-C")
	if i < 0 || slices.Index(argv, "do") != i+2 {
		return nil, fmt.Errorf("unexpected argv %q", argv)
	}
	target := argv[i+1]
	f.targets = append(f.targets, target)
	if target == "lazyrex-hub" {
		var ids []string
		for id := range f.clients {
			ids = append(ids, id)
		}
		switch {
		case len(ids) > 1:
			return nil, errors.New(`exit status 3: Error: client target "lazyrex-hub" is ambiguous`)
		case len(ids) == 0 || f.clients[ids[0]] == "lost":
			return nil, errors.New(`exit status 3: Error: "lazyrex-hub" is waiting to reconnect`)
		}
		target = ids[0]
	}
	switch f.clients[target] {
	case "connected":
		return json.Marshal(map[string]any{"by": target})
	case "lost":
		return nil, fmt.Errorf(`exit status 3: Error: %q is waiting to reconnect`, target)
	}
	return nil, fmt.Errorf(`exit status 3: Error: client target %q not found`, target)
}

func (f *fakePeerServer) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists, slices.Clone(f.targets)
}

func fakePeerCall(t *testing.T, h *Hub) string {
	t.Helper()
	out, err := h.peerCall(context.Background(), h.cfg.Peers[0], "hub.peer.pull", map[string]any{"hub": "a"})
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		By string `json:"by"`
	}
	json.Unmarshal(out, &r)
	return r.By
}

// TestPeerCallClientID (deploy-2bbca-mesh1): peerCall names the peer's one connected lazyrex-hub
// client by its ID and lists the clients only for the first call and after a failed one. After a
// restart (the old client lost, a new one connected) the call with the kept ID fails, and the same
// call runs again on the new hub: no wait for Rex to drop the lost client.
func TestPeerCallClientID(t *testing.T) {
	h := peerHub(t, "a", "b", nil)
	f := &fakePeerServer{listOK: true, clients: map[string]string{"client:old": "connected"}}
	h.peers.rex = f.run
	for range 3 {
		if by := fakePeerCall(t, h); by != "client:old" {
			t.Fatalf("by %q", by)
		}
	}
	if lists, targets := f.counts(); lists != 1 || !slices.Equal(targets, []string{"client:old", "client:old", "client:old"}) {
		t.Fatalf("steady: %d lists, targets %v; want one list and three calls by ID", lists, targets)
	}

	f.set(map[string]string{"client:old": "lost", "client:new": "connected"}) // the peer's hub restarted
	if by := fakePeerCall(t, h); by != "client:new" {
		t.Fatalf("after the restart: by %q", by)
	}
	if by := fakePeerCall(t, h); by != "client:new" {
		t.Fatalf("the next call: by %q", by)
	}
	if lists, targets := f.counts(); lists != 2 || !slices.Equal(targets[3:], []string{"client:old", "client:new", "client:new"}) {
		t.Fatalf("restart: %d lists, targets %v; want one more list, then the new ID kept", lists, targets[3:])
	}

	f.set(map[string]string{"client:new": "connected"}) // Rex dropped the lost client: nothing changes
	fakePeerCall(t, h)
	if lists, _ := f.counts(); lists != 2 {
		t.Fatalf("%d lists after Rex dropped the lost client, want 2", lists)
	}
}

// A failure of a call with the kept ID that is not a restart (the list names the same hub) is not
// run twice, and its error stays the call's error.
func TestPeerCallNoSecondTryOnSameHub(t *testing.T) {
	h := peerHub(t, "a", "b", nil)
	f := &fakePeerServer{listOK: true, clients: map[string]string{"client:one": "connected"}}
	h.peers.rex = f.run
	fakePeerCall(t, h)
	refused := 0
	h.peers.rex = func(ctx context.Context, argv []string) ([]byte, error) {
		if slices.Contains(argv, "do") {
			f.run(ctx, argv)
			refused++
			return nil, errors.New("exit status 1: Error: refused: unknown_hub")
		}
		return f.run(ctx, argv)
	}
	_, err := h.peerCall(context.Background(), h.cfg.Peers[0], "hub.peer.pull", map[string]any{"hub": "a"})
	if err == nil || refused != 1 {
		t.Fatalf("err %v, %d calls; want the refusal after one call", err, refused)
	}
	if lists, _ := f.counts(); lists != 2 {
		t.Fatalf("%d lists, want 2 (the first call, then the check after the failure)", lists)
	}
}

// Without exactly one connected lazyrex-hub client (two connected, none, or a failed list), the call
// names the label as before, keeps no ID, and a label error stays "ambiguous" for the 2 s retry.
func TestPeerCallLabelFallback(t *testing.T) {
	for _, c := range []struct {
		name    string
		listOK  bool
		clients map[string]string
		ok      bool
	}{
		{"two connected", true, map[string]string{"client:x": "connected", "client:y": "connected"}, false},
		{"only lost", true, map[string]string{"client:x": "lost"}, false},
		{"failed list", false, map[string]string{"client:x": "connected"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := peerHub(t, "a", "b", nil)
			f := &fakePeerServer{listOK: c.listOK, clients: c.clients}
			h.peers.rex = f.run
			_, err := h.peerCall(context.Background(), h.cfg.Peers[0], "hub.peer.pull", map[string]any{"hub": "a"})
			if (err == nil) != c.ok {
				t.Fatalf("err %v", err)
			}
			if _, targets := f.counts(); !slices.Equal(targets, []string{"lazyrex-hub"}) {
				t.Fatalf("targets %v, want the label once", targets)
			}
			if c.name == "two connected" && !peerAmbiguous(err) {
				t.Fatalf("%v is not the ambiguous error", err)
			}
			if l := h.peerLink("b"); l.hubID != "" {
				t.Fatalf("kept %q", l.hubID)
			}
		})
	}
}

// Calls from the pull, the sync and the agents read run at the same time. After a restart none of
// them keeps the old ID: every later call goes to the new hub.
func TestPeerCallConcurrentRestart(t *testing.T) {
	h := peerHub(t, "a", "b", nil)
	f := &fakePeerServer{listOK: true, clients: map[string]string{"client:old": "connected"}}
	h.peers.rex = f.run
	fakePeerCall(t, h)
	f.set(map[string]string{"client:old": "lost", "client:new": "connected"})
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			if _, err := h.peerCall(context.Background(), h.cfg.Peers[0], "hub.peer.agents", map[string]any{"hub": "a"}); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	_, before := f.counts()
	for range 3 {
		if by := fakePeerCall(t, h); by != "client:new" {
			t.Fatalf("by %q", by)
		}
	}
	if _, after := f.counts(); !slices.Equal(after[len(before):], []string{"client:new", "client:new", "client:new"}) {
		t.Fatalf("after the restart: %v", after[len(before):])
	}
}

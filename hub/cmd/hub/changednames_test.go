package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// changedNames gives h one registered plugin and records the hub.changed names that reach it.
// The returned wait fails the test unless every wanted name arrives within two seconds.
func changedNames(t *testing.T, h *Hub) (wait func(want ...string)) {
	t.Helper()
	f := newChangedFake(t)
	if h.plugins == nil {
		h.plugins = map[string]*Plugin{}
	}
	h.plugins["p"] = &Plugin{Name: "p", ClientID: "client:p"}
	if h.vs == nil {
		h.vs = &viewSvc{dirty: map[string]bool{}, workers: map[string]chan struct{}{}}
	}
	h.vs.workers["p"] = make(chan struct{}, 1) // no view worker runs
	return func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			f.mu.Lock()
			var got []string
			for _, c := range f.calls {
				got = append(got, strings.TrimPrefix(c, "client:p "))
			}
			f.mu.Unlock()
			missing := slices.DeleteFunc(slices.Clone(want), func(w string) bool { return slices.Contains(got, w) })
			if len(missing) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("hub.changed %v was not delivered (delivered: %v)", missing, got)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

package main

import (
	"encoding/json"
	"testing"
)

// The call record keeps the last testCallsKept calls of each plugin. A read after a dropped call
// says dropped; a read after the last dropped seq does not. after and action filter; next is the
// last seq of any plugin.
func TestCallRecord(t *testing.T) {
	h := &Hub{test: newTestState(), plugins: map[string]*Plugin{"p": {Name: "p"}, "q": {Name: "q"}}}
	h.testRecordCall("q", "q-1", "hub.store.get", json.RawMessage(`{"keys":["k"]}`))
	for i := 0; i < testCallsKept+1; i++ {
		action := "hub.store.get"
		if i == testCallsKept {
			action = "hub.store.scan"
		}
		h.testRecordCall("p", "p-1", action, nil)
	}
	read := func(args map[string]any) map[string]any {
		t.Helper()
		out, err := h.testCalls(args)
		if err != nil {
			t.Fatal(err)
		}
		return out.(map[string]any)
	}
	all := read(map[string]any{"plugin": "p"})
	calls := all["calls"].([]testCall)
	if len(calls) != testCallsKept || all["dropped"] != true || all["next"] != int64(testCallsKept+2) {
		t.Fatalf("all calls: %d, dropped %v, next %v", len(calls), all["dropped"], all["next"])
	}
	if calls[0].Seq != 3 || string(calls[0].Args) != "{}" || calls[0].InstanceID != "p-1" {
		t.Errorf("the oldest kept call: %+v", calls[0])
	}
	if out := read(map[string]any{"plugin": "p", "after": 2.0}); out["dropped"] != false || len(out["calls"].([]testCall)) != testCallsKept {
		t.Errorf("after the dropped seq: %v", out["dropped"])
	}
	scans := read(map[string]any{"plugin": "p", "action": "hub.store.scan", "after": "2"})["calls"].([]testCall)
	if len(scans) != 1 || scans[0].Seq != int64(testCallsKept+2) {
		t.Errorf("the scans: %+v", scans)
	}
	q := read(map[string]any{"plugin": "q"})
	if qc := q["calls"].([]testCall); len(qc) != 1 || string(qc[0].Args) != `{"keys":["k"]}` || q["dropped"] != false {
		t.Errorf("another plugin: %+v", q)
	}
	for _, args := range []map[string]any{{}, {"plugin": "nope"}, {"plugin": "p", "after": true}, {"plugin": "p", "after": "x"}, {"plugin": "p", "after": 1.5}, {"plugin": "p", "after": -1.0}, {"plugin": "p", "after": "-1"}} {
		if _, err := h.testCalls(args); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

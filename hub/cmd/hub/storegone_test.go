package main

import (
	"context"
	"encoding/json"
	"testing"
)

// The actions of the removed hub.store, the store test API and sql_return get the plain unknown
// action reply (D5 of the hub.store removal): the hub declares none of them, and dispatch has no
// case for them. The caller is a registered plugin instance, the only caller that reached
// them before.
func TestStoreActionsAreGone(t *testing.T) {
	gone := []string{"hub.store.get", "hub.store.scan", "hub.store.op", "hub.store.ops_scan", "hub.store.batch",
		"hub.admin.sql_return", "hub.test.store", "hub.test.write", "hub.test.op.put", "hub.test.rev"}
	declared := map[string]bool{}
	for _, a := range hubClientActions(true) {
		name, _ := a["name"].(string)
		declared[name] = true
	}
	h, _ := workerTestHub(t)
	h.test, h.vs = &testState{}, newViewSvc()
	h.plugins = map[string]*Plugin{"p": {Name: "p", ClientID: "c-p"}}
	for _, action := range gone {
		if declared[action] {
			t.Errorf("the hub still declares %s", action)
		}
		p := perform{Action: action, Args: json.RawMessage(`{"keys":["k"],"ops":[]}`)}
		p.Requester.ClientID = "c-p"
		_, err := h.dispatch(context.Background(), p)
		if err == nil || err.Error() != "unknown action "+action {
			t.Errorf("%s: %v, want unknown action", action, err)
		}
	}
}

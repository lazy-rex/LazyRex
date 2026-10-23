package main

import (
	"context"
	"testing"
)

// hub.workers reports a degraded read when a Rex read of an open worker's pane fails: its process
// (the state shows "exited") or its program status (the status shows nil). A good read, also one
// with no child and no status, is not degraded.
func TestWorkersReadDegraded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		procErr   string // the error code of terminal.process
		statusErr string // the error code of terminal.program_status
		noChild   bool   // terminal.process reports no child
		records   []any
		degraded  bool
		state     string
		status    any
	}{
		{name: "good", records: []any{map[string]any{"id": "", "state": "idle", "msg": "ok"}}, state: "running",
			status: map[string]any{"state": "idle", "msg": "ok"}},
		{name: "empty status", records: []any{}, state: "running"},
		{name: "no child", noChild: true, records: []any{}, state: "exited"},
		{name: "process fails", procErr: "internal", records: []any{}, degraded: true, state: "exited"},
		{name: "status fails", statusErr: "internal", degraded: true, state: "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := workerTestHub(t)
			h.ui = &uiState{kick: make(chan struct{}, 1), sess: map[string]string{"s": uiSessionKey}} // no session.attach
			mustExec(t, h, `UPDATE spawns SET status = 'open', agent_id = '', session_id = 's', window_id = 'w', block_id = 'b'`)
			h.rex = replyRex(t, func(method string) (any, string) {
				switch method {
				case "session.list_blocks":
					return map[string]any{"blocks": []any{map[string]any{"block_id": "b", "window_id": "w"}}}, ""
				case "com.superlogical.terminal.process":
					if tc.procErr != "" {
						return nil, tc.procErr
					}
					if tc.noChild {
						return map[string]any{"child": nil}, ""
					}
					return map[string]any{"child": map[string]any{"pid": 4242}}, ""
				case "com.superlogical.terminal.program_status":
					if tc.statusErr != "" {
						return nil, tc.statusErr
					}
					return map[string]any{"records": tc.records}, ""
				}
				return map[string]any{}, ""
			})
			got, degraded, err := h.workersRead(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			if degraded != tc.degraded {
				t.Errorf("degraded = %v, want %v", degraded, tc.degraded)
			}
			ws := got.(map[string]any)["workers"].([]any)
			if len(ws) != 1 {
				t.Fatalf("workers = %v, want one", ws)
			}
			w := ws[0].(map[string]any)
			if w["state"] != tc.state {
				t.Errorf("state = %v, want %v", w["state"], tc.state)
			}
			if st, _ := w["status"].(map[string]any); (tc.status == nil) != (st == nil) || (st != nil && st["msg"] != "ok") {
				t.Errorf("status = %v, want %v", w["status"], tc.status)
			}
		})
	}
}

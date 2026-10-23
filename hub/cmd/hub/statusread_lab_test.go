package main

import (
	"context"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// TestStatusReadNeedsAttach records a Rex fact on a lab server: a connection reads a pane's
// program status (com.superlogical.terminal.program_status) only after it attaches the pane's
// session. Lab only (rexLab: HARNESS_LAB=1).
func TestStatusReadNeedsAttach(t *testing.T) {
	socket := rexLab(t).socket
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner := labCaller(t, socket)
	var created struct {
		SessionID string `json:"session_id"`
	}
	cmd := []string{"/bin/sh", "-c", `printf '\033]7501;state=idle\033\\'; exec sleep 600`}
	err := owner.Call(ctx, "session.create", map[string]any{"label": "status-read-probe", "initial_windows": []any{map[string]any{
		"window_label": "probe", "layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "options": map[string]any{"command": cmd}}}}}}, &created)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		owner.Call(context.Background(), "session.destroy", map[string]any{"session_id": created.SessionID}, nil)
	})
	if err := owner.Call(ctx, "session.attach", map[string]any{"session_id": created.SessionID, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	var blocks struct {
		Blocks []struct {
			BlockID string `json:"block_id"`
		} `json:"blocks"`
	}
	if err := owner.Call(ctx, "session.list_blocks", map[string]any{"session_id": created.SessionID}, &blocks); err != nil || len(blocks.Blocks) != 1 {
		t.Fatalf("blocks %v %v", blocks, err)
	}
	block := blocks.Blocks[0].BlockID
	time.Sleep(time.Second) // the printf runs

	reader := labCaller(t, socket)
	var st struct {
		Records []struct{ ID, State string } `json:"records"`
	}
	before := reader.BlockCall(ctx, "com.superlogical.terminal.program_status", created.SessionID, block, nil, &st)
	t.Logf("before attach: err %v, records %+v", before, st.Records)
	if err := reader.Call(ctx, "session.attach", map[string]any{"session_id": created.SessionID, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	st.Records = nil
	after := reader.BlockCall(ctx, "com.superlogical.terminal.program_status", created.SessionID, block, nil, &st)
	t.Logf("after attach: err %v, records %+v", after, st.Records)
	if before == nil {
		t.Errorf("the read before attach succeeded: the attach is not needed")
	}
	if after != nil || len(st.Records) != 1 || st.Records[0].ID != "" || st.Records[0].State != "idle" {
		t.Errorf("after attach: %v %+v", after, st.Records)
	}
}

// labCaller dials socket and drains its events; the cleanup closes it.
func labCaller(t *testing.T, socket string) *rex.Rex {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := rex.Dial(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	go func() {
		for range r.Events {
		}
	}()
	return r
}

package main

import (
	"context"
	"testing"
)

// A form's end opens its return panel; the user's next command arrives before the panel's renderer
// attaches (Tunnels forward.lua test 8, 2026-10-19). The lab renderer only sleeps, so the panel never
// attaches: the order is fixed without timing.

// A view that never attached holds no user input. A replacement takes its place without asking it.
func TestSlotReplacesNeverAttachedViewLab(t *testing.T) {
	h, _, sess, win := panelsLabHub(t)
	ctx := context.Background()
	if out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-first", Plugin: "p", Route: "/panels/a", Ctx: panel(sess, win)}); err != nil || out["status"] != "opened" {
		t.Fatalf("first panel: %v %v", out, err)
	}
	out, err := h.openSlot(ctx, slotRequest{ReplID: "repl-second", Plugin: "p", Route: "/panels/b", Ctx: panel(sess, win)})
	if err != nil || out["status"] != "opened" {
		t.Fatalf("replacing a panel that never attached: %v %v", out, err)
	}
	if st := replState(t, h, "repl-second"); st != "done" {
		t.Fatalf("state %s", st)
	}
}

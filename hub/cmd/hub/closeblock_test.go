package main

import (
	"context"
	"testing"
)

// A block whose session is gone is closed: block.close answers session_not_found, and closeBlock
// reports no error, so closeMoved does not try again forever (live hub, 2026-10-26).
func TestCloseBlockSessionGone(t *testing.T) {
	for _, code := range []string{"not_found", "session_not_found"} {
		h := formCloseHub(t, 0, code)
		if err := h.closeBlock(context.Background(), "s", "block-0"); err != nil {
			t.Errorf("%s: closeBlock: %v, want nil", code, err)
		}
	}
	h := formCloseHub(t, 0, "internal")
	if err := h.closeBlock(context.Background(), "s", "block-0"); err == nil {
		t.Error("internal: closeBlock: nil, want the error")
	}
}

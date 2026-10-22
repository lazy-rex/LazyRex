package main

import (
	"context"
	"slices"
	"testing"
	"time"
)

// A renderer that answers released or confirm for a row that another path ended meanwhile froze
// for it, and the other path's view.resume can have reached it first: askRelease adds exactly one
// view.resume, opens nothing, and leaves the row ended (row 7, rule 3). A row that moved on to
// another live state has another owner: askRelease sends nothing more.
func TestReleaseAnswerForAnEndedRowResumesOnce(t *testing.T) {
	for _, answer := range []string{"released", "confirm"} {
		for _, moved := range []string{"cancelled", "superseded", "opening"} {
			t.Run(answer+"/"+moved, func(t *testing.T) {
				h := releaseTestHub(t)
				ctx := context.Background()
				h.viewsAttachState(ctx)
				h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, client_id, attachment, attach_state) VALUES ('v', 'p', ?, '/panels/a', 'open', 'c', 'att', 'attached')`, formTab)
				h.db.Exec(`INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, state, next_title, created)
					VALUES ('R', 's', 'w', 'v', 'n', 'p', '/panels/b', '{}', 'releasing', 'Next', '')`)
				fr := newPerformRex(t, func(c performCall) (any, string) {
					if c.Action == "view.release" {
						// Another path ends the row while the renderer answers.
						h.db.Exec(`UPDATE ui_replacements SET state = ? WHERE repl_id = 'R'`, moved)
						return map[string]any{"status": answer}, ""
					}
					return map[string]any{"status": "resumed"}, ""
				})
				h.rex = fr.dial(t)
				r, err := h.slotRow(ctx, h.db, "R")
				if err != nil {
					t.Fatal(err)
				}
				if err := h.askRelease(ctx, r); err != nil {
					t.Fatalf("askRelease: %v", err)
				}
				want := []string{"view.release", "view.resume"}
				if moved == "opening" {
					want = want[:1]
				}
				for deadline := time.Now().Add(3 * time.Second); len(fr.actions()) < len(want) && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				}
				time.Sleep(300 * time.Millisecond) // no further call follows
				if got := fr.actions(); !slices.Equal(got, want) {
					t.Fatalf("calls %v, want %v", got, want)
				}
				// No new pane: the hub made no request other than these client.perform calls.
				if m := fr.methodsSeen(); len(m) != len(want) || slices.ContainsFunc(m, func(s string) bool { return s != "client.perform" }) {
					t.Fatalf("requests %v, want only %d client.perform", m, len(want))
				}
				var state string
				h.db.QueryRow(`SELECT state FROM ui_replacements WHERE repl_id = 'R'`).Scan(&state)
				if state != moved {
					t.Fatalf("the row is %q, want %q", state, moved)
				}
			})
		}
	}
}

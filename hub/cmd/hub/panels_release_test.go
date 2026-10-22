package main

import (
	"context"
	"strings"
	"testing"
)

// releaseTestHub has the views and replacement tables of an older hub: views has no attach_state.
func releaseTestHub(t *testing.T) *Hub {
	t.Helper()
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(uiSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(panelSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`ALTER TABLE views ADD COLUMN context TEXT NOT NULL DEFAULT ''`); err != nil { // forms.go
		t.Fatal(err)
	}
	return h
}

// hub.view.replacement reads the named replacement of the caller's view, never another one, and
// changes nothing.
func TestViewReplacementReadsTheNamedReplacement(t *testing.T) {
	h := releaseTestHub(t)
	ctx := context.Background()
	h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, client_id, attachment, attach_state) VALUES ('v', 'p', ?, '/panels/a', 'open', 'c', 'att', 'attached')`, formTab)
	h.db.Exec(`INSERT INTO views (view_id, plugin, tab, route, status, client_id, attachment, attach_state) VALUES ('closed', 'p', ?, '/panels/a', 'closed', 'c', 'att', 'attached')`, formTab)
	for _, row := range [][]string{{"r-old", "v", "cancelled"}, {"r-new", "v", "opening"}, {"r-other", "other", "opening"}} {
		h.db.Exec(`INSERT INTO ui_replacements (repl_id, session_id, window_id, old_view, new_view, plugin, route, context, state, next_title, created)
			VALUES (?, 's', 'w', ?, 'n', 'p', '/panels/b', '{}', ?, 'Next', '')`, row[0], row[1], row[2])
	}
	dump := func() string {
		var out []string
		rows, _ := h.db.Query(`SELECT repl_id || state FROM ui_replacements UNION ALL SELECT view_id || status || attach_state || attachment FROM views ORDER BY 1`)
		defer rows.Close()
		for rows.Next() {
			var s string
			rows.Scan(&s)
			out = append(out, s)
		}
		return strings.Join(out, ",")
	}
	before := dump()
	var p perform
	p.Requester.ClientID = "c"
	read := func(view, att, repl string) (any, error) {
		return h.viewReplacement(ctx, p, map[string]any{"view_id": view, "attachment": att, "replacement_id": repl})
	}
	out, err := read("v", "att", "r-old")
	got, _ := out.(map[string]any)["replacement"].(map[string]any)
	if err != nil || got["replacement_id"] != "r-old" || got["state"] != "cancelled" {
		t.Fatalf("the named replacement: %v %v", out, err)
	}
	for _, repl := range []string{"r-other", "r-missing"} {
		out, err := read("v", "att", repl)
		if err != nil || out.(map[string]any)["replacement"] != nil {
			t.Fatalf("%s: want null, got %v %v", repl, out, err)
		}
	}
	if _, err := read("v", "stale", "r-old"); err == nil || !strings.Contains(err.Error(), "stale_attachment") {
		t.Fatalf("a stale attachment: %v", err)
	}
	if _, err := read("closed", "att", "r-old"); err == nil || !strings.Contains(err.Error(), "stale_view") {
		t.Fatalf("a closed view: %v", err)
	}
	if _, err := read("v", "att", ""); err == nil {
		t.Fatal("no replacement_id was accepted")
	}
	if after := dump(); after != before {
		t.Fatalf("the read changed state:\n%s\n%s", before, after)
	}
}

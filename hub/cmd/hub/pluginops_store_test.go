package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestPluginOpStoreSchema: an attempt row has the binding and proof columns with empty defaults,
// and plugin_hold keeps at most one row.
func TestPluginOpStoreSchema(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	if _, err := h.db.Exec(`INSERT INTO plugin_op_attempts (attempt, op_id, call, at) VALUES ('a1', 'op1', 'start', 1)`); err != nil {
		t.Fatal(err)
	}
	var bound, inst, block, intent, sent, proof, by string
	err := h.db.QueryRow(`SELECT bound_at, target_instance, target_block, intent_id, sent_at, proof, resolved_by
		FROM plugin_op_attempts WHERE attempt = 'a1'`).Scan(&bound, &inst, &block, &intent, &sent, &proof, &by)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{bound, inst, block, intent, sent, proof, by} {
		if v != "" {
			t.Fatalf("new attempt has a non-empty binding or proof column: %q", v)
		}
	}
	if _, err := h.db.Exec(`INSERT INTO plugin_hold (one, release_id, acquired_at) VALUES (1, 'r1', 't1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO plugin_hold (one, release_id, acquired_at) VALUES (1, 'r2', 't2')`); err == nil {
		t.Fatal("a second hold row was stored")
	}
	if _, err := h.db.Exec(`INSERT INTO plugin_hold (one, release_id, acquired_at) VALUES (2, 'r2', 't2')`); err == nil {
		t.Fatal("a hold row with one = 2 was stored")
	}
}

// The B8 probe rows (lab/probes/b8-terminal-rows.sql) fit the current schema, and they are terminal
// and proved: the hold reads none, with nothing inflight or unresolved.
func TestB8ProbeRows(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "lab", "probes", "b8-terminal-rows.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(string(b)); err != nil {
		t.Fatal(err)
	}
	res, err := h.holdStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := res.(map[string]any)
	if s["state"] != "none" || len(s["inflight"].([]map[string]any)) != 0 || len(s["unresolved"].([]map[string]any)) != 0 {
		t.Fatalf("the probe rows: %v", s)
	}
}

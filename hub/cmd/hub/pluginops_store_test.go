package main

import (
	"testing"
)

// TestPluginOpStoreSchema: an attempt row has the binding and proof columns with empty defaults,
// and the removed release hold has no table.
func TestPluginOpStoreSchema(t *testing.T) {
	h, _, _ := opsTestHub(t, "")
	if _, err := h.db.Exec(`INSERT INTO plugin_op_attempts (attempt, op_id, call, at) VALUES ('a1', 'op1', 'start', 1)`); err != nil {
		t.Fatal(err)
	}
	var bound, inst, block, intent, sent, proof string
	err := h.db.QueryRow(`SELECT bound_at, target_instance, target_block, intent_id, sent_at, proof
		FROM plugin_op_attempts WHERE attempt = 'a1'`).Scan(&bound, &inst, &block, &intent, &sent, &proof)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{bound, inst, block, intent, sent, proof} {
		if v != "" {
			t.Fatalf("new attempt has a non-empty binding or proof column: %q", v)
		}
	}
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'plugin_hold'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plugin_hold: %d tables, %v", n, err)
	}
}

package main

import (
	"context"
	"testing"
)

func TestNoCycleExitRetryDoesNotExitLaterCycle(t *testing.T) {
	h, _ := workerTestHub(t)
	if _, err := h.db.Exec(uiSchema + appModeSchema); err != nil {
		t.Fatal(err)
	}
	h.ui = &uiState{kick: make(chan struct{}, 1)}
	ctx := context.Background()
	args := map[string]any{"for": "user", "key": "exit-before-enter"}
	first, err := h.uiExit(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if first.(map[string]any)["phase"] != "none" {
		t.Fatal(first)
	}
	if _, err := h.db.Exec(`INSERT INTO ui_cycles(cycle,seq,phase,created,updated) VALUES ('later',1,'active','now','now')`); err != nil {
		t.Fatal(err)
	}
	retry, err := h.uiExit(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if retry.(map[string]any)["phase"] != "none" {
		t.Fatalf("Old Exit changed the later cycle: %v", retry)
	}
	var phase string
	if err := h.db.QueryRow(`SELECT phase FROM ui_cycles WHERE cycle='later'`).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if phase != "active" {
		t.Fatalf("Later cycle changed to %s", phase)
	}
}

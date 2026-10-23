package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// Codex's interleaving (review of 7e91929): two IDs of one agent (an identity and a spawn address
// bound to it) both look a key up and find nothing, then both admit it. The check inside the
// admission transaction refuses the second. Also a bind between the first admission and the second
// lookup.
func TestSpawnAddressAdmissionRace(t *testing.T) {
	ctx := context.Background()
	for _, aliasFirst := range []bool{true, false} {
		h, _ := workerTestHub(t)
		if _, err := h.db.Exec(identitySchema + spawnWindowSchema); err != nil {
			t.Fatal(err)
		}
		alias := func() {
			if _, err := h.db.Exec(`INSERT INTO spawns (spawn_id,caller,key,request,profile,title,agent_id,parent,status,created,identity) VALUES ('alice','user','k-alice','{}','agent','Agent','spawn:alice','user','open','now','claude:review')`); err != nil {
				t.Fatal(err)
			}
		}
		if aliasFirst {
			alias()
		}
		callers := []string{"spawn:alice", "claude:review"}
		for _, caller := range callers {
			row, err := h.callerKeyRow(ctx, "spawns", "spawn_id", caller, "race")
			if err != nil {
				t.Fatal(err)
			}
			var id string
			if err := row.Scan(&id); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("lookup: %v", err)
			}
		}
		cwd, err := realDir(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		admit := func(i int, caller string) error {
			tx, err := h.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			s := &spawnRow{ID: []string{"race-one", "race-two"}[i], Caller: caller, Key: "race", Profile: "shell", Parent: caller, Status: "sending", Created: "now", Session: "lab-session", Cwd: cwd}
			if err := h.admitSpawn(ctx, tx, s, Profile{ID: "shell", Kind: "accessory", Name: "Shell"}, "", `{}`, map[string]string{}); err != nil {
				return err
			}
			if err := keyTakenIn(ctx, tx, "spawn_windows", caller, "racew"); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO spawn_windows VALUES (?, 'racew', '{}', 's', '[]', 'now')`, caller); err != nil {
				t.Fatal(err)
			}
			return tx.Commit()
		}
		if err := admit(0, "spawn:alice"); err != nil {
			t.Fatal(err)
		}
		if !aliasFirst {
			alias() // the bind comes after the first admission and before the second
		}
		if err := admit(1, "claude:review"); !errors.Is(err, errKeyTaken) {
			t.Fatalf("aliasFirst=%v: second admission: %v", aliasFirst, err)
		}
		var spawns, windows int
		h.db.QueryRow(`SELECT COUNT(*) FROM spawns WHERE key = 'race'`).Scan(&spawns)
		h.db.QueryRow(`SELECT COUNT(*) FROM spawn_windows WHERE key = 'racew'`).Scan(&windows)
		if spawns != 1 || windows != 1 {
			t.Fatalf("aliasFirst=%v: %d spawn rows, %d window rows", aliasFirst, spawns, windows)
		}
		// The window check alone: a second window row of the same agent is refused too.
		tx, _ := h.db.BeginTx(ctx, nil)
		if err := keyTakenIn(ctx, tx, "spawn_windows", "claude:review", "racew"); !errors.Is(err, errKeyTaken) {
			t.Fatalf("window check: %v", err)
		}
		tx.Rollback()
	}
}

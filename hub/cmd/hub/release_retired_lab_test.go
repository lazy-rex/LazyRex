package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The retired schema in a real rehearsal (lead-hubdrop). release freeze needs H on origin/main, so
// this runs once a hub with retiredSchema drops is on main (2d). OLD is setLabOld, which has
// records, ops, sql_returns, sql_notice_plugins and ui_tabs.rev_known; H is RELEASE_LAB_RETIRED_H,
// a commit on origin/main whose hub drops them through retiredSchema. RELEASE_LAB_RETIRED_WANT is
// "pass", or a text that the failed rehearsal must contain. A mutant H is never on origin/main, so
// its check is TestReleaseRetiredMutantLab. An OLD binary, which has no export, must read as
// absent (TestReleaseRetiredExportOldLab).
func TestReleaseRehearseLabRetired(t *testing.T) {
	h, want := os.Getenv("RELEASE_LAB_RETIRED_H"), os.Getenv("RELEASE_LAB_RETIRED_WANT")
	if os.Getenv("RELEASE_LAB") != "1" || h == "" || want == "" {
		t.Skip("set RELEASE_LAB=1, RELEASE_LAB_RETIRED_H and RELEASE_LAB_RETIRED_WANT to run the retired schema lab")
	}
	bin := filepath.Join(t.TempDir(), "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	f := t.TempDir()
	labFixtureAt(t, f, bin, setLabOld, h, nil)
	run := filepath.Join(f, "run")
	lab := filepath.Join("/tmp", fmt.Sprintf("rexlab-release-t%dretired", os.Getpid()))
	logp, err := labRehearse(t, bin, f, run, lab, nil)
	if want != "pass" {
		if err == nil {
			t.Fatalf("the rehearsal of %s passed; want a failure with %q (log %s)", h, want, logp)
		}
		if !strings.Contains(string(mustReadFile(logp)), want) {
			t.Fatalf("the rehearsal of %s failed without %q: %v (log %s; the lab %s is kept)", h, want, err, logp, lab)
		}
		t.Logf("the rehearsal of %s failed as wanted: %q (log %s; the lab %s is kept)", h, want, logp, lab)
		return
	}
	if err != nil {
		t.Fatalf("rehearse: %v (log %s; the lab %s is kept)", err, logp, lab)
	}
	var rec relRehearsal
	if err := json.Unmarshal(mustReadFile(filepath.Join(run, relRehearsalFile)), &rec); err != nil {
		t.Fatal(err)
	}
	r := rec.Retired
	if !rec.Pass || rec.Tier != tierFull || r == nil || r.Absent || r.Commit != h || len(r.BinSHA256) != 64 {
		t.Fatalf("record: pass %v tier %s %v, retired %+v", rec.Pass, rec.Tier, rec.TierReasons, r)
	}
	names := map[string]bool{}
	for _, e := range r.Entries {
		names[e.name()] = true
	}
	for _, n := range []string{"records", "ops", "sql_returns", "sql_notice_plugins", "ui_tabs.rev_known"} {
		if !names[n] {
			t.Errorf("record: the retired entries %+v do not name %s", r.Entries, n)
		}
	}
	t.Logf("retired schema of %s: %+v; tier reasons %q", h, r, rec.TierReasons)
}

// A real older hub binary: OLD built from setLabOld has no export, so it reads as absent (exit 2,
// the unknown-flag line first). It runs without a rehearsal.
func TestReleaseRetiredExportOldLab(t *testing.T) {
	if os.Getenv("RELEASE_LAB") != "1" {
		t.Skip("set RELEASE_LAB=1 to build the OLD hub of the set lab")
	}
	if _, _, absent, err := exportRetired(buildHubAt(t, setLabOld), t.TempDir()); err != nil || !absent {
		t.Errorf("the export of OLD %s: absent %v, %v", setLabOld, absent, err)
	}
}

// buildHubAt builds the hub of commit (it must be in this clone) and returns the binary.
func buildHubAt(t *testing.T, commit string) string {
	t.Helper()
	src := t.TempDir()
	if out, err := exec.Command("sh", "-c", fmt.Sprintf("git -C ../../.. archive %s hub | tar -x -C %s", commit, src)).CombinedOutput(); err != nil {
		t.Fatalf("archive %s: %v\n%s", commit, err, out)
	}
	bin := filepath.Join(t.TempDir(), "hub-"+commit)
	c := exec.Command("go", "build", "-o", bin, "./cmd/hub")
	c.Dir = filepath.Join(src, "hub")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", commit, err, out)
	}
	return bin
}

// The mutant check of the release gate, without a rehearsal (release freeze refuses an H that is
// not on origin/main, and a mutant never is). RELEASE_LAB_RETIRED_H and RELEASE_LAB_RETIRED_MUTANT
// are two commits in this clone; the mutant's export is H's without RELEASE_LAB_RETIRED_LEFT. On a
// lab hub.db with OLD tables, the drops of H's real export are applied. With H's export the schema
// check passes; with the mutant's real export it fails on the left-out table or column. LIMIT: the
// OLD tables are made by the test (retiredLab), not by the OLD hub, and the test applies the drops
// from H's export, not H's own open. The rehearsal of the real candidate covers both.
func TestReleaseRetiredMutantLab(t *testing.T) {
	hc, mc, leftName := os.Getenv("RELEASE_LAB_RETIRED_H"), os.Getenv("RELEASE_LAB_RETIRED_MUTANT"), os.Getenv("RELEASE_LAB_RETIRED_LEFT")
	if os.Getenv("RELEASE_LAB") != "1" || hc == "" || mc == "" || leftName == "" {
		t.Skip("set RELEASE_LAB=1, RELEASE_LAB_RETIRED_H, _MUTANT and _LEFT (the left-out entry, for example sql_returns) to run the mutant check")
	}
	export := func(commit string) []retiredEntry {
		exp, _, absent, err := exportRetired(buildHubAt(t, commit), t.TempDir())
		if err != nil || absent {
			t.Fatalf("the export of %s: absent %v, %v", commit, absent, err)
		}
		return exp.Retired
	}
	hEntries, mEntries := export(hc), export(mc)
	// The mutant's export must be H's, in order and in every field, with exactly the entry named
	// RELEASE_LAB_RETIRED_LEFT left out, so that no other change can give the wanted failure
	// (lead-c49mut, codex-26).
	var left, rest []retiredEntry
	for _, e := range hEntries {
		if e.name() == leftName {
			left = append(left, e)
		} else {
			rest = append(rest, e)
		}
	}
	if len(left) != 1 {
		t.Fatalf("H %s names %q %d times in %+v; want once", hc, leftName, len(left), hEntries)
	}
	if !reflect.DeepEqual(rest, mEntries) {
		t.Fatalf("the export of the mutant %s is %+v; want the export of H %s without %s: %+v", mc, mEntries, hc, leftName, rest)
	}
	check := func(entries []retiredEntry) error {
		h := retiredLab(t, entries)
		db, err := h.hubDB(false)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range hEntries {
			q := "DROP TABLE " + e.Table
			if e.Column != "" {
				q = "ALTER TABLE " + e.Table + " DROP COLUMN " + e.Column
			}
			if !hasRetired(h.schemas["before"], e) {
				continue
			}
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		db.Close()
		return h.schemaAt("forward", false)
	}
	if err := check(hEntries); err != nil {
		t.Errorf("with the export of H %s: %v", hc, err)
	}
	err := check(mEntries)
	want := "a column of OLD changed or went: column " + left[0].Table + "."
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("with the export of the mutant %s (leaves out %+v): %v, want %q", mc, left, err, want)
	}
	t.Logf("H %s: %+v; the mutant %s leaves out %+v: %v", hc, hEntries, mc, left, err)
}

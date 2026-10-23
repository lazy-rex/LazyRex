package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The retired schema in a real rehearsal (lead-hubdrop): OLD is setLabOld, which has records, ops,
// sql_returns, sql_notice_plugins and ui_tabs.rev_known; H is RELEASE_LAB_RETIRED_H, a pushed
// commit whose hub drops them through retiredSchema. RELEASE_LAB_RETIRED_WANT is "pass", or a text
// that the failed rehearsal must contain (a mutant H). An OLD binary, which has no export, must
// read as absent.
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

	// A real older hub binary: OLD built from setLabOld has no export.
	src := t.TempDir()
	if out, err := exec.Command("sh", "-c", fmt.Sprintf("git -C ../../.. archive %s hub | tar -x -C %s", setLabOld, src)).CombinedOutput(); err != nil {
		t.Fatalf("archive %s: %v\n%s", setLabOld, err, out)
	}
	oldBin := filepath.Join(t.TempDir(), "hub-old")
	c := exec.Command("go", "build", "-o", oldBin, "./cmd/hub")
	c.Dir = filepath.Join(src, "hub")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build OLD: %v\n%s", err, out)
	}
	if _, _, absent, err := exportRetired(oldBin, t.TempDir()); err != nil || !absent {
		t.Errorf("the export of OLD %s: absent %v, %v", setLabOld, absent, err)
	}
}

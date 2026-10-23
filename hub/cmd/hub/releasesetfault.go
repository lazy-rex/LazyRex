package main

// The fault cases of a set rehearsal (plan r3, section 5). They run for the first plugin of the
// set, at FROM, before its real forward. Each is evidence of recovery only: the record keeps them
// apart from the success evidence of the real TO trees ("faults", not "set"), a fault commit is
// never a TO, and the live gate never reads them.
//   - failed-forward: a lab-made commit on top of TO whose main file fails at load, only in the lab
//     mirror. Its forward must fail at replace or verify; the rollback must pass; then the data
//     checks and the probes on FROM.
//   - interrupted-replace: the real TO. RELEASE_TEST_STOP=replace-<to12>:after stops the replace
//     step after the hub call and before the end record (exit 3). The journal must end the op with
//     its begin record, and the hub must run TO (the call happened). A rerun of replace and verify
//     must pass; then the data checks, the rollback, and the probes on FROM.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// relSetFault is one fault run.
type relSetFault struct {
	Plugin string   `json:"plugin"`
	Case   string   `json:"case"`           // failed-forward or interrupted-replace
	Commit string   `json:"commit"`         // the commit of the run: the fault commit, or the real TO
	Base   string   `json:"base,omitempty"` // failed-forward: the TO under the fault commit
	Steps  []string `json:"steps"`          // "<step> exit <code>", in order
	Result string   `json:"result"`
}

// setFaults runs both fault cases for e.
func (h *rehearsal) setFaults(e *relSetEntry) error {
	if err := h.faultFailedForward(e); err != nil {
		return relFail("the fault case failed-forward of %s: %v", e.Plugin, err)
	}
	if err := h.faultInterruptedReplace(e); err != nil {
		return relFail("the fault case interrupted-replace of %s: %v", e.Plugin, err)
	}
	return nil
}

// faultRun freezes a lab plugin run of e to commit in dir, and returns its run directory.
func (h *rehearsal) faultRun(e *relSetEntry, name, commit string) (string, error) {
	base := filepath.Join(h.lab, "set", e.Plugin, name)
	run := filepath.Join(base, "run")
	spec := map[string]any{"kind": "plugin", "plugin": e.Plugin, "to": commit, "config": h.cfg, "server": h.l.server, "lab": true}
	specPath := filepath.Join(base, "spec.json")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	if err := writeJSON(specPath, spec); err != nil {
		return "", err
	}
	if err := h.child("freeze", nil, "-spec", specPath, "-run", run); err != nil {
		return "", err
	}
	return run, nil
}

func (h *rehearsal) faultFailedForward(e *relSetEntry) (err error) {
	fc, restore, err := h.faultCommit(e)
	if err != nil {
		return err
	}
	defer func() {
		if rerr := restore(); rerr != nil && err == nil {
			err = rerr
		}
	}()
	f := relSetFault{Plugin: e.Plugin, Case: "failed-forward", Commit: fc, Base: e.To}
	run, err := h.faultRun(e, "fault-forward", fc)
	if err != nil {
		return err
	}
	failed := ""
	for _, st := range []string{"precheck", "backup", "pull", "replace", "verify"} {
		code, logp, err := h.childCode(st, nil, "-run", run)
		if err != nil {
			return err
		}
		f.Steps = append(f.Steps, fmt.Sprintf("%s exit %d", st, code))
		if code == relOK {
			continue
		}
		if code != relFailed {
			return relFail("step %s exited %d, not %d; see %s", st, code, relFailed, logp)
		}
		failed = st
		break
	}
	switch failed {
	case "":
		return relFail("the forward to the broken commit %s passed every step", fc)
	case "replace", "verify":
	default:
		return relFail("the forward to the broken commit failed at %s, before replace: it proves no recovery", failed)
	}
	if err := h.child("rollback", nil, "-run", run); err != nil {
		return err
	}
	f.Steps = append(f.Steps, "rollback exit 0")
	if err := h.setChecks(e.Plugin + " fault failed-forward rollback"); err != nil {
		return err
	}
	if err := h.runItems(e, e.Probe, "fault-forward", true); err != nil {
		return relFail("the probes on FROM after the failed forward: %v", err)
	}
	if err := h.setChecks(e.Plugin + " fault failed-forward probes on FROM"); err != nil {
		return err
	}
	f.Result = "pass"
	h.set.faults = append(h.set.faults, f)
	return nil
}

func (h *rehearsal) faultInterruptedReplace(e *relSetEntry) error {
	f := relSetFault{Plugin: e.Plugin, Case: "interrupted-replace", Commit: e.To}
	run, err := h.faultRun(e, "fault-replace", e.To)
	if err != nil {
		return err
	}
	for _, st := range []string{"precheck", "backup", "pull"} {
		if err := h.child(st, nil, "-run", run); err != nil {
			return err
		}
		f.Steps = append(f.Steps, st+" exit 0")
	}
	op := "replace-" + e.To[:12]
	code, logp, err := h.childCode("replace", []string{"RELEASE_TEST_STOP=" + op + ":after"}, "-run", run)
	if err != nil {
		return err
	}
	f.Steps = append(f.Steps, fmt.Sprintf("replace (stop %s:after) exit %d", op, code))
	if code != relTestStop {
		return relFail("the stopped replace exited %d, not %d; see %s", code, relTestStop, logp)
	}
	// The journal: the last record of the op is its begin (no end, no failed record).
	recs, err := (&releaser{run: run}).journal()
	if err != nil {
		return err
	}
	last := ""
	for _, x := range recs {
		if x.Op == op {
			last = x.Phase
		}
	}
	if last != "begin" {
		return relFail("after the stop, the last record of %s is %q, not an open begin", op, last)
	}
	// The call happened: the hub runs TO.
	if err := h.withLab(func(lr *releaser) error {
		hub, err := lr.currentHub()
		if err != nil {
			return err
		}
		entries, _, err := lr.hubPlugins(hub.Client)
		if err != nil {
			return err
		}
		x := entryOf(entries, e.Plugin)
		if x == nil || x.Commit != e.To {
			b, _ := json.Marshal(x)
			return relFail("after the stopped replace, the hub does not run TO: %s", b)
		}
		return nil
	}); err != nil {
		return err
	}
	f.Steps = append(f.Steps, "the hub runs TO; the journal has an open begin of "+op)
	for _, st := range []string{"replace", "verify"} {
		if err := h.child(st, nil, "-run", run); err != nil {
			return relFail("the rerun: %v", err)
		}
		f.Steps = append(f.Steps, st+" (rerun) exit 0")
	}
	if err := h.setChecks(e.Plugin + " fault interrupted-replace rerun"); err != nil {
		return err
	}
	if err := h.child("rollback", nil, "-run", run); err != nil {
		return err
	}
	f.Steps = append(f.Steps, "rollback exit 0")
	if err := h.setChecks(e.Plugin + " fault interrupted-replace rollback"); err != nil {
		return err
	}
	if err := h.runItems(e, e.Probe, "fault-replace", true); err != nil {
		return relFail("the probes on FROM after the rollback: %v", err)
	}
	if err := h.setChecks(e.Plugin + " fault interrupted-replace probes on FROM"); err != nil {
		return err
	}
	f.Result = "pass"
	h.set.faults = append(h.set.faults, f)
	return nil
}

// faultCommit makes the fault commit of e in the lab mirror of its repository: TO with a first
// line in the main file that fails at load. The tracked branch of the lab mirror points at it
// (a freeze needs TO on that branch) until restore puts the branch back.
func (h *rehearsal) faultCommit(e *relSetEntry) (string, func() error, error) {
	labCfg, err := readPluginCmdConfig(h.cfg)
	if err != nil {
		return "", nil, err
	}
	l, err := readLock(labCfg.lock)
	if err != nil {
		return "", nil, err
	}
	var le *lockEntry
	for _, x := range l.Plugins {
		if x.Name == e.Plugin {
			le = x
		}
	}
	if le == nil || le.RefKind != "branch" {
		return "", nil, relFail("the lab lock has no branch entry named %s", e.Plugin)
	}
	mirror := filepath.Join(h.lab, "mirrors", le.Owner, le.Repo+".git")
	tmp := filepath.Join(h.lab, "tmp", "fault-"+e.Plugin)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", nil, err
	}
	git := func(env []string, args ...string) (string, error) {
		c := exec.Command("git", append([]string{"-C", mirror}, args...)...)
		c.Env = append(append(append([]string{}, h.env...), "GIT_AUTHOR_NAME=release lab", "GIT_AUTHOR_EMAIL=lab@invalid",
			"GIT_COMMITTER_NAME=release lab", "GIT_COMMITTER_EMAIL=lab@invalid"), env...)
		out, err := c.CombinedOutput()
		if err != nil {
			return "", relFail("git %s in %s: %v: %s", strings.Join(args, " "), mirror, err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
	mb, err := git(nil, "show", e.To+":"+filepath.Join(le.Subdir, "lazyrex.json"))
	if err != nil {
		return "", nil, err
	}
	var m struct {
		Main string `json:"main"`
	}
	if err := json.Unmarshal([]byte(mb), &m); err != nil || m.Main == "" {
		return "", nil, relFail("the manifest of %s at TO has no main file (%v)", e.Plugin, err)
	}
	mainPath := filepath.Join(le.Subdir, m.Main)
	orig, err := git(nil, "show", e.To+":"+mainPath)
	if err != nil {
		return "", nil, err
	}
	broken := filepath.Join(tmp, "main.lua")
	if err := os.WriteFile(broken, []byte("error('release lab fault: a TO whose main file fails at load')\n"+orig+"\n"), 0o600); err != nil {
		return "", nil, err
	}
	blob, err := git(nil, "hash-object", "-w", broken)
	if err != nil {
		return "", nil, err
	}
	idx := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	if _, err := git(idx, "read-tree", e.To); err != nil {
		return "", nil, err
	}
	if _, err := git(idx, "update-index", "--cacheinfo", "100644,"+blob+","+mainPath); err != nil {
		return "", nil, err
	}
	tree, err := git(idx, "write-tree")
	if err != nil {
		return "", nil, err
	}
	fc, err := git(nil, "commit-tree", tree, "-p", e.To, "-m", "release lab fault: "+m.Main+" fails at load (lab mirror only)")
	if err != nil {
		return "", nil, err
	}
	ref := "refs/heads/" + le.Ref
	head, err := git(nil, "rev-parse", ref)
	if err != nil {
		return "", nil, err
	}
	if _, err := git(nil, "update-ref", ref, fc, head); err != nil {
		return "", nil, err
	}
	h.say("the fault commit %s of %s (on TO %s); the lab mirror branch %s points at it until the case ends", fc, e.Plugin, e.To, le.Ref)
	restore := func() error {
		_, err := git(nil, "update-ref", ref, head, fc)
		return err
	}
	return fc, restore, nil
}

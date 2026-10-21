package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A process information read that fails makes the caller's pane unknown. The hub.admin.* guard
// and the hub.plugins.reload guard refuse such a caller before any change: an admin action needs
// a verified caller.
func TestAdminGuardsRefuseUnknownAncestry(t *testing.T) {
	h, _ := workerTestHub(t)
	dir := t.TempDir()
	h.cfg.managedPath = filepath.Join(dir, "plugins.json")
	writeManaged(t, dir, `{"plugins":[`) // malformed: a reload that runs reports it
	h.agents, h.plugins = map[string]*Agent{}, map[string]*Plugin{"p": {Name: "p", Desired: "running"}}
	h.procRead = func(pid int) (procRecord, error) { return procRecord{}, errors.New("injected read failure") }
	pid := os.Getpid()
	want := "cannot check the caller's process ancestry: process " + fmt.Sprint(pid) + ": injected read failure; admin actions need a verified caller"
	for _, action := range []string{"hub.admin.start", "hub.admin.stop", "hub.admin.rebind", "hub.admin.unlock", "hub.plugins.reload"} {
		var p perform
		json.Unmarshal([]byte(fmt.Sprintf(`{"action":%q,"args":{"plugin":"p"},"requester":{"client_id":"c9","principal":{"pid":%d}}}`, action, pid)), &p)
		_, err := h.dispatch(context.Background(), p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", action, err)
		}
	}
	if h.managedErr != nil || h.plugins["p"].Desired != "running" {
		t.Errorf("a refused call changed the hub: managed error %v, desired %s", h.managedErr, h.plugins["p"].Desired)
	}
}

// The walk has three outcomes: a match, a verified answer without a match (the chain reaches pid
// 1), and an incomplete walk (a read error, a process that ended, or more than 64 steps). A match
// found before the walk fails still counts.
func TestCallerInManagedPaneOutcomes(t *testing.T) {
	h, _ := workerTestHub(t)
	parents := map[int]int{40: 30, 30: 20, 20: 1}
	h.procRead = func(pid int) (procRecord, error) {
		if pid == 99 {
			return procRecord{}, errProcGone
		}
		if pid >= 1000 {
			return procRecord{ppid: pid + 1}, nil // an endless chain
		}
		pp, ok := parents[pid]
		if !ok {
			return procRecord{}, errors.New("no read")
		}
		return procRecord{ppid: pp}, nil
	}
	h.agents = map[string]*Agent{}
	h.plugins = map[string]*Plugin{"p": {Name: "p", ChildPID: 30}}
	for _, c := range []struct {
		pid     int
		in      bool
		errText string
	}{
		{40, true, ""},
		{30, true, ""},
		{20, false, ""},
		{99, false, "process 99: no such process"},
		{1000, false, "the walk passed 64 processes"},
		{55, false, "process 55: no read"},
	} {
		in, err := h.callerInManagedPane(c.pid)
		if in != c.in || (c.errText == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.errText)) {
			t.Errorf("pid %d: %v, %v", c.pid, in, err)
		}
	}
	// A match before a failing read still counts: 43 -> 30 (the plugin child), and the read of 30
	// fails.
	parents[43] = 30
	delete(parents, 30)
	if in, err := h.callerInManagedPane(43); !in || err != nil {
		t.Errorf("pid 43: %v, %v", in, err)
	}
}

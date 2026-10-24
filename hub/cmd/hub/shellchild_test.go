package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// procTable is a fake process table for procRead: pid -> record; a missing pid is gone.
func procTable(t map[int]procRecord, fail map[int]bool) func(int) (procRecord, error) {
	return func(pid int) (procRecord, error) {
		if fail[pid] {
			return procRecord{}, errors.New("permission denied")
		}
		r, ok := t[pid]
		if !ok {
			return procRecord{}, errProcGone
		}
		return r, nil
	}
}

// agentProcess follows the leader of the pane terminal's foreground group only when the reporter
// runs under it and it runs under the pane child (lead-irgap4); otherwise the pane child (0, "").
func TestAgentProcess(t *testing.T) {
	// 10 the pane child (a shell), 20 the CLI it runs in the foreground, 30 a hook, 31 the reporter;
	// 40 another foreground job of the shell; 50 a process outside the pane.
	base := func() map[int]procRecord {
		return map[int]procRecord{
			10: {ppid: 1, start: "1.000010", tpgid: 20},
			20: {ppid: 10, start: "1.000020", tpgid: 20},
			30: {ppid: 20, start: "1.000030"},
			31: {ppid: 30, start: "1.000031"},
			40: {ppid: 10, start: "1.000040"},
			50: {ppid: 1, start: "1.000050"},
		}
	}
	for name, c := range map[string]struct {
		edit      func(map[int]procRecord)
		fail      map[int]bool
		pid       int
		wantStart string
	}{
		"a CLI under an interactive shell": {pid: 20, wantStart: "1.000020"},
		"a spawned CLI is the pane child":  {edit: func(m map[int]procRecord) { m[10] = procRecord{ppid: 1, start: "1.000010", tpgid: 10} }},
		"no terminal":                      {edit: func(m map[int]procRecord) { m[10] = procRecord{ppid: 1, start: "1.000010", tpgid: -1} }},
		"the reporter is not under the foreground job": {edit: func(m map[int]procRecord) {
			m[10] = procRecord{ppid: 1, start: "1.000010", tpgid: 40}
		}},
		"the foreground leader is not under the pane child": {edit: func(m map[int]procRecord) {
			m[10] = procRecord{ppid: 1, start: "1.000010", tpgid: 50}
			m[30] = procRecord{ppid: 50, start: "1.000030"}
		}},
		"the foreground leader is a zombie": {edit: func(m map[int]procRecord) { m[20] = procRecord{ppid: 10, start: "1.000020", zombie: true} }},
		"the pane child read fails":         {fail: map[int]bool{10: true}},
		"the foreground leader read fails":  {fail: map[int]bool{20: true}},
	} {
		m := base()
		if c.edit != nil {
			c.edit(m)
		}
		pid, start := agentProcess(procTable(m, c.fail), 31, 10)
		if pid != c.pid || start != c.wantStart {
			t.Errorf("%s: (%d, %q), want (%d, %q)", name, pid, start, c.pid, c.wantStart)
		}
	}
}

// identityLoop's check ends a conversation whose agent CLI ended (gone, another start, a zombie)
// and keeps it on a live CLI or a failed read; it does not check a pane that follows its pane
// child (that exit has its Rex event). The start check (identityRecover) checks that pane too.
func TestIdentityCheckFollowsTheCLI(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	if err := h.workersSchemaStart(ctx); err != nil {
		t.Fatal(err)
	}
	const start = "1.000001"
	table := map[int]procRecord{
		102: {start: "2.000002"},
		103: {start: start, zombie: true},
		104: {start: start},
		900: {start: start}, // every pane child: the shell stays
	}
	fail := map[int]bool{105: true}
	rows := map[string]int{"gone": 101, "restarted": 102, "zombie": 103, "live": 104, "unread": 105, "child": 0}
	h.idents = map[string]*Agent{}
	for name, cli := range rows {
		id, block, cs := "claude:"+name, "block:"+name, start
		child := 900
		if cli == 0 {
			child, cs = 106, "" // follows a pane child that is gone; no CLI recorded
		}
		h.db.Exec(`INSERT INTO identity_panes (block_id, session_id, child_pid, child_start, kind, current, cli_pid, cli_start) VALUES (?, 'session:1', ?, ?, 'claude', ?, ?, ?)`,
			block, child, start, id, cli, cs)
		h.db.Exec(`INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated) VALUES (?, 'claude', ?, 'current', ?, '', '', 'x', 'x')`, id, name, block)
		h.idents[id] = &Agent{ID: id, BlockID: block, ChildPID: cli, delivered: map[string]int64{}}
	}
	h.procRead = procTable(table, fail)
	if err := h.identityCheck(ctx, false); err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{"live": true, "unread": true, "child": true}
	for name := range rows {
		want := ""
		if keep[name] {
			want = "claude:" + name
		}
		if got, err := h.paneCurrent(ctx, "block:"+name); err != nil || got != want {
			t.Errorf("loop, %s: current %q (%v), want %q", name, got, err, want)
		}
	}
	var reason string
	h.db.QueryRow(`SELECT detail FROM identity_log WHERE identity = 'claude:gone' AND event = 'exited'`).Scan(&reason)
	if reason != `{"reason":"agent process exited"}` {
		t.Errorf("the exit detail is %q", reason)
	}
	// The start check reads the pane child of a pane without a CLI: gone, so it ends.
	if err := h.identityRecover(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.paneCurrent(ctx, "block:child"); got != "" {
		t.Errorf("start, child: current %q, want none", got)
	}
	for _, name := range []string{"live", "unread"} {
		if got, _ := h.paneCurrent(ctx, "block:"+name); got != "claude:"+name {
			t.Errorf("start, %s: current %q", name, got)
		}
	}
}

// The conversation follows the CLI in memory too: identitySync loads cli_pid into Agent.ChildPID
// when it is set, and the pane child otherwise.
func TestIdentitySyncLoadsTheCLI(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	if err := h.workersSchemaStart(ctx); err != nil {
		t.Fatal(err)
	}
	for i, cli := range []int{0, 77} {
		id, block := fmt.Sprintf("claude:s%d", i), fmt.Sprintf("block:s%d", i)
		h.db.Exec(`INSERT INTO identity_panes (block_id, session_id, child_pid, child_start, kind, current, cli_pid, cli_start) VALUES (?, 'session:1', 66, '1.000066', 'claude', ?, ?, ?)`,
			block, id, cli, map[int]string{0: "", 77: "1.000077"}[cli])
		h.db.Exec(`INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated) VALUES (?, 'claude', ?, 'current', ?, '', '', 'x', 'x')`, id, fmt.Sprint(i), block)
		h.idents[id] = &Agent{ID: id, BlockID: block, ChildPID: map[int]int{0: 66, 77: 77}[cli], delivered: map[string]int64{}}
	}
	if err := h.identitySync(ctx); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, want := range map[string]struct {
		pid   int
		start string
	}{"claude:s0": {66, "1.000066"}, "claude:s1": {77, "1.000077"}} {
		a := h.idents[id]
		if a == nil || a.ChildPID != want.pid || a.ChildStart != want.start {
			t.Errorf("%s: %+v, want pid %d start %s", id, a, want.pid, want.start)
		}
	}
}

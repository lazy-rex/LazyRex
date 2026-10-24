package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// worktreeLab starts a lab Rex server (rexLab: LAB=1) and returns a 45 s context and the
// lab. worktreeDial opens one more connection to it.
func worktreeLab(t *testing.T) (context.Context, *lab) {
	t.Helper()
	l := rexLab(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	return ctx, l
}

func worktreeDial(t *testing.T, ctx context.Context, l *lab) *rex.Rex {
	t.Helper()
	r, err := rex.Dial(ctx, l.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// Exercise the actual operation consumer over Rex, with a controlled plugin response.
// A running answer must preserve the stored request and prevent later operations overtaking it.
func TestWorktreeRunningViewActionLab(t *testing.T) {
	ctx, l := worktreeLab(t)
	r, plugin := worktreeDial(t, ctx, l), worktreeDial(t, ctx, l)
	go func() {
		for range r.Events {
		}
	}()
	var welcome struct {
		Client struct {
			ID string `json:"id"`
		} `json:"client"`
	}
	if err := json.Unmarshal(plugin.Welcome, &welcome); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Call(ctx, "client.keymap", map[string]any{"actions": []any{
		map[string]any{"name": "view.action", "kind": "client", "title": "Action"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	replies := make(chan map[string]any, 4)
	requests := make(chan perform, 4)
	errors := make(chan error, 4)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case raw, ok := <-plugin.Events:
				if !ok {
					return
				}
				var frame struct {
					Payload struct {
						Type    string  `json:"type"`
						Payload perform `json:"payload"`
					} `json:"payload"`
				}
				if json.Unmarshal(raw, &frame) != nil || frame.Payload.Type != "client_perform_requested" {
					continue
				}
				p := frame.Payload.Payload
				requests <- p
				select {
				case <-ctx.Done():
					return
				case reply := <-replies:
					reply["call_id"] = p.CallID
					if err := plugin.Call(ctx, "client.reply", reply, nil); err != nil {
						errors <- err
					}
				}
			}
		}
	}()
	h, _ := workerTestHub(t)
	h.rex, h.cfg.Server = r, l.server
	h.plugins = map[string]*Plugin{"worktrees": {Name: "worktrees", ClientID: welcome.Client.ID}}
	if err := h.viewsStart(ctx); err != nil {
		t.Fatal(err)
	}
	input := `{"route":"/forms/new-worktree","node_id":"new_worktree","action":"submit","operation_key":"create-one","input":{"values":{"repo":"/tmp/repo","branch":"feature"}}}`
	for _, key := range []string{"create-one", "later"} {
		if _, err := h.db.Exec(`INSERT INTO view_ops (plugin,user,op_key,view_id,kind,fingerprint,args,status) VALUES ('worktrees','user',?,'view-one','action','fingerprint',?,'pending')`, key, input); err != nil {
			t.Fatal(err)
		}
	}
	replies <- map[string]any{"result": map[string]any{"status": "running", "close_form": true}}
	if !h.runOps(ctx, "worktrees") {
		t.Fatal("running operation did not request a retry")
	}
	first := <-requests
	var state, result, effect string
	if err := h.db.QueryRow(`SELECT status,result,effect_state FROM view_ops WHERE op_key='create-one'`).Scan(&state, &result, &effect); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || result != "" || effect != "" {
		t.Fatalf("running committed a terminal result or effect: %q %q %q", state, result, effect)
	}
	if len(requests) != 0 {
		t.Fatal("a later operation overtook running work")
	}
	// hub.session.ensure uses outcome_unknown after a lost create reply. Preserve the key
	// instead of inviting a fresh submit that could create a second session.
	replies <- map[string]any{"error": map[string]any{"code": "outcome_unknown", "message": "session create reply lost"}}
	if !h.runOps(ctx, "worktrees") {
		t.Fatal("an explicitly unknown outcome must remain pending")
	}
	<-requests
	if err := h.db.QueryRow(`SELECT status FROM view_ops WHERE op_key='create-one'`).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("unknown outcome: %s %v", state, err)
	}
	// A fresh hub object reads the same durable operation after an interrupted attempt.
	h2 := &Hub{db: h.db, ev: h.ev, rex: r, plugins: h.plugins, vs: newViewSvc()}
	replies <- map[string]any{"result": map[string]any{"path": "/tmp/tree", "created": true}}
	replies <- map[string]any{"error": map[string]any{"code": "refused", "message": "lab refusal"}}
	if h2.runOps(ctx, "worktrees") {
		t.Fatal("terminal results should not request a retry")
	}
	retry := <-requests
	var a, b any
	json.Unmarshal(first.Args, &a)
	json.Unmarshal(retry.Args, &b)
	if first.Action != retry.Action || !reflect.DeepEqual(a, b) {
		t.Fatalf("retry changed stored request: %s / %s", first.Args, retry.Args)
	}
	<-requests // the later operation runs only after the first finishes
	if err := h.db.QueryRow(`SELECT status FROM view_ops WHERE op_key='create-one'`).Scan(&state); err != nil || state != "succeeded" {
		t.Fatalf("completion: %s %v", state, err)
	}
	if err := h.db.QueryRow(`SELECT status FROM view_ops WHERE op_key='later'`).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("refusal: %s %v", state, err)
	}
	if h2.runOps(ctx, "worktrees") || len(requests) != 0 {
		t.Fatal("completed operations were dispatched again")
	}
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
}

// Advice sees unmanaged terminal panes and workers, but creates no lock or spawn record.
func TestWorktreeCwdCheckLab(t *testing.T) {
	ctx, l := worktreeLab(t)
	r := worktreeDial(t, ctx, l)
	go func() {
		for range r.Events {
		}
	}()
	h, _ := workerTestHub(t)
	h.rex, h.cfg.Server = r, l.server
	h.ui = &uiState{sess: map[string]string{}}
	if _, err := h.db.Exec(`DELETE FROM spawns`); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	var session struct {
		ID string `json:"session_id"`
	}
	if err := r.Call(ctx, "session.create", map[string]any{"label": "worktrees-cwd-check"}, &session); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Call(context.Background(), "session.destroy", map[string]any{"session_id": session.ID}, nil) })
	if err := r.Call(ctx, "session.attach", map[string]any{"session_id": session.ID, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	var block struct {
		ID string `json:"block_id"`
	}
	if err := r.Call(ctx, "session.new_block", map[string]any{"session_id": session.ID, "flavor": "com.superlogical.terminal.shell",
		"options": map[string]any{"cwd": filepath.Join(tree, "sub"), "command": []string{"/bin/sleep", "60"}, "login": false}}, &block); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) map[string]any {
		t.Helper()
		out, err := h.cwdCheck(ctx, map[string]any{"path": tree})
		if err != nil {
			t.Fatal(err)
		}
		got := out.(map[string]any)
		if got["available"] != want {
			t.Fatalf("available want %v: %v", want, got)
		}
		var locks int
		if err := h.db.QueryRow(`SELECT COUNT(*) FROM cwd_locks`).Scan(&locks); err != nil || locks != 0 {
			t.Fatalf("check created a lock: %d %v", locks, err)
		}
		return got
	}
	if len(check(false)["panes"].([]any)) != 1 {
		t.Fatal("unmanaged live pane was not returned")
	}
	if err := r.Call(ctx, "block.close", map[string]any{"session_id": session.ID, "block_id": block.ID}, nil); err != nil {
		t.Fatal(err)
	}
	check(true)
	if _, err := h.db.Exec(`INSERT INTO spawns (spawn_id,caller,key,request,profile,title,parent,status,created,cwd) VALUES ('unknown','user','k','{}','p','P','user','sending','now','')`); err != nil {
		t.Fatal(err)
	}
	if len(check(false)["workers"].([]any)) != 1 {
		t.Fatal("unknown worker cwd did not block advice")
	}
	if _, err := h.db.Exec(`UPDATE spawns SET status='failed'`); err != nil {
		t.Fatal(err)
	}
	check(true)
	// The advice did not reserve the path: the actual lock remains a separate operation.
	out, err := h.cwdLock(ctx, "worktrees", map[string]any{"path": tree, "key": "remove"})
	if err != nil || out.(map[string]any)["locked"] != true {
		t.Fatalf("lock after advice: %v %v", out, err)
	}
}

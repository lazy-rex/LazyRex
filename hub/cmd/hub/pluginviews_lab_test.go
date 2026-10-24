package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// labView is a view of the home plugin in a lab, read and used through its test renderer
// (lazyrex-hub testview).
type labView struct {
	s   *startLab
	ctx context.Context
	id  string
}

type labViewNode struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Items []struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Detail string `json:"detail"`
	} `json:"items"`
}

// openPluginsPanel opens the real Plugins… panel of the home plugin from a pane, as the Rex palette
// does: hub.palette.run takes the caller's client ID in ctx, as Rex sets it for a palette action.
// Our client attaches to the pane's session (session.list_blocks needs that).
func openPluginsPanel(t *testing.T, s *startLab, ctx context.Context) *labView {
	t.Helper()
	pc, err := rex.Dial(ctx, s.l.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		for range pc.Events {
		}
	}()
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := pc.Call(ctx, "session.create", map[string]any{"label": "c8-palette", "initial_windows": []any{map[string]any{"window_label": "c8-palette",
		"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "options": map[string]any{"command": []string{"/bin/sleep", "100000"}, "login": false}}}}}}, &created); err != nil {
		t.Fatal(err)
	}
	if err := pc.Call(ctx, "session.attach", map[string]any{"session_id": created.SessionID, "event_scope": "session"}, nil); err != nil {
		t.Fatal(err)
	}
	var blocks struct {
		Blocks []struct {
			BlockID string `json:"block_id"`
		} `json:"blocks"`
	}
	if err := pc.Call(ctx, "session.list_blocks", map[string]any{"session_id": created.SessionID}, &blocks); err != nil || len(blocks.Blocks) != 1 {
		t.Fatalf("the palette pane: %v %v", blocks, err)
	}
	label := fmt.Sprintf("c8-palette-%d", time.Now().UnixNano())
	if err := pc.Call(ctx, "client.set_label", map[string]any{"label": label}, nil); err != nil {
		t.Fatal(err)
	}
	pid, err := s.c.resolveClient(ctx, label)
	if err != nil {
		t.Fatal(err)
	}
	var run json.RawMessage
	if err := pc.Call(ctx, "client.perform", map[string]any{"client_id": s.hub, "action": "hub.palette.run", "timeout_ms": 10000, "args": map[string]any{
		"command": "lazyrex.plugins", "ctx": map[string]any{"client_id": pid, "session_id": created.SessionID, "block_id": blocks.Blocks[0].BlockID}}}, &run); err != nil {
		t.Fatalf("hub.palette.run: %v", err)
	}
	return s.viewAt(ctx, "/panels/plugins")
}

// viewAt waits for the attached view of the home plugin at route.
func (s *startLab) viewAt(ctx context.Context, route string) *labView {
	s.t.Helper()
	var id string
	s.wait("the view "+route, func() bool {
		var out struct {
			Views []struct {
				ViewID   string `json:"view_id"`
				Route    string `json:"route"`
				Attached bool   `json:"attached"`
			} `json:"views"`
		}
		b, _ := json.Marshal(s.must("hub.test.views", map[string]any{"plugin": "lazyrex"}))
		json.Unmarshal(b, &out)
		for _, v := range out.Views {
			if v.Route == route && v.Attached {
				id = v.ViewID
			}
		}
		return id != ""
	})
	return &labView{s: s, ctx: ctx, id: id}
}

// get is the renderer's snapshot: its revision and its nodes (nil when it does not answer yet).
func (v *labView) get() (json.RawMessage, []labViewNode) {
	tv, err := v.s.c.resolveClient(v.ctx, "lazyrex-testview:"+v.id)
	if err != nil {
		return nil, nil
	}
	raw, err := v.s.c.perform(v.ctx, tv, "testview.get", nil)
	if err != nil {
		return nil, nil
	}
	var snap struct {
		Revision json.RawMessage `json:"revision"`
		Tree     struct {
			Nodes []labViewNode `json:"nodes"`
		} `json:"tree"`
	}
	json.Unmarshal(raw, &snap)
	return snap.Revision, snap.Tree.Nodes
}

// raw is the renderer's snapshot as text, for a failure message.
func (v *labView) raw() string {
	tv, err := v.s.c.resolveClient(v.ctx, "lazyrex-testview:"+v.id)
	if err != nil {
		return err.Error()
	}
	b, err := v.s.c.perform(v.ctx, tv, "testview.get", nil)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// waitNode is wait with the snapshot in the timeout message.
func (v *labView) waitNode(what string, ok func() bool) {
	v.s.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(150 * time.Millisecond) {
		if time.Now().After(deadline) {
			v.s.t.Fatalf("timeout: %s; the snapshot: %s", what, v.raw())
		}
	}
}

// node is the first node whose ID starts with prefix, or nil.
func (v *labView) node(prefix string) *labViewNode {
	_, nodes := v.get()
	for i := range nodes {
		if strings.HasPrefix(nodes[i].ID, prefix) {
			return &nodes[i]
		}
	}
	return nil
}

// detail is the detail of the item id of the list node list, or "".
func (v *labView) detail(list, id string) string {
	if n := v.node(list); n != nil && n.ID == list {
		for _, i := range n.Items {
			if i.ID == id {
				return i.Detail
			}
		}
	}
	return ""
}

// renders gives the render attempts of the view, and if a plugins change marks it: with no
// declaration, or no changes list in it, every change marks the view (docs/hub.md, deps).
func (v *labView) renders() (attempts int, tracks bool) {
	var out struct {
		Views []struct {
			ViewID   string `json:"view_id"`
			Attempts int    `json:"attempts"`
			Declared *struct {
				Changes *[]string `json:"changes"`
			} `json:"declared"`
		} `json:"views"`
	}
	b, _ := json.Marshal(v.s.must("hub.test.renders", map[string]any{"plugin": "lazyrex"}))
	json.Unmarshal(b, &out)
	for _, x := range out.Views {
		if x.ViewID == v.id {
			d := x.Declared
			return x.Attempts, d == nil || d.Changes == nil || contains(*d.Changes, "plugins")
		}
	}
	return 0, false
}

// action sends a user's view action at the snapshot's revision, as the renderer does, and waits
// until the operation is final and its effects are not pending (hub.test.operation).
func (v *labView) action(node, action string, input map[string]any, key string) map[string]any {
	v.s.t.Helper()
	rev, _ := v.get()
	tv, err := v.s.c.resolveClient(v.ctx, "lazyrex-testview:"+v.id)
	if err != nil {
		v.s.t.Fatal(err)
	}
	if _, err := v.s.c.perform(v.ctx, tv, "testview.action", map[string]any{"node_id": node, "action": action, "input": input,
		"revision": rev, "operation_key": key}); err != nil {
		v.s.t.Fatalf("%s %s: %v", node, action, err)
	}
	var st map[string]any
	v.s.wait("the operation "+key, func() bool {
		st = v.s.must("hub.test.operation", map[string]any{"plugin": "lazyrex", "operation_key": key})
		return st["status"] != "pending" && st["effects"] != "pending"
	})
	return st
}

// waitLong is wait with the bound d: a plugin sync runs once a minute (peerSyncEvery).
func (s *startLab) waitLong(what string, d time.Duration, ok func() bool) {
	s.t.Helper()
	for deadline := time.Now().Add(d); !ok(); time.Sleep(300 * time.Millisecond) {
		if time.Now().After(deadline) {
			s.t.Fatalf("timeout: %s", what)
		}
	}
}

// The real Plugins… panel of the home plugin, opened from a pane as the Rex palette does, follows a
// lock change with no action (lead-c8-b2 item 4, positive tracking): a GitHub entry runs commit A;
// the manager's update pins B and replaces the instance; the open list renders again by itself,
// with the running commit B; after a stop, it shows "pinned B". Lab only: LAZYREX_LAB=1.
func TestPluginsPanelFollowsLockLab(t *testing.T) {
	gitTestEnv(t)
	r := newTestRepo(t)
	first := r.commit(map[string]string{"lazyrex.json": `{"name":"gh","main":"main.lua"}`, "main.lua": fmt.Sprintf(startFixture, "gh")})
	const src = "github:lab/r@main"
	s := newStartLabHome(t, labGitEnv)
	s.labRepos(map[string]*testRepo{"r": r})
	s.setConfig("plugins", []any{map[string]any{"source": s.o.home.dir}})
	s.writeManagedFile(`{"plugins":[{"id":"gh","source":"` + src + `"}]}`)
	s.start() // the hub installs gh at its start
	s.startRun("gh")
	s.registered("lazyrex")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	p := openPluginsPanel(t, s, ctx)
	detail := func() string { return p.detail("entries", "managed:gh") }
	s.wait("the list shows commit A", func() bool { return strings.Contains(detail(), first[:12]) })
	if d := detail(); strings.Contains(d, "pinned") || !strings.Contains(d, src) {
		t.Fatalf("before the update: %q", d)
	}
	before, tracks := p.renders()
	if !tracks {
		t.Fatal("a plugins change does not mark the panel")
	}

	second := r.commit(map[string]string{"main.lua": fmt.Sprintf(startFixture, "gh") + "-- second\n"})
	if op := s.opDone(s.opsAgent("alice"), "up-gh", map[string]any{"op": "update", "entry": "managed:gh", "input": map[string]any{"to": "@"}}); op["phase"] != "done" {
		t.Fatalf("update: %v", op)
	}
	s.wait("the open list shows the running commit B", func() bool { return strings.Contains(detail(), second[:12]) })
	if d := detail(); strings.Contains(d, first[:12]) || strings.Contains(d, "pinned") {
		t.Fatalf("the row still shows commit A, or a pin: %q", d)
	}
	// A stopped entry has no running commit: the row shows the pin B.
	s.must("hub.admin.stop", map[string]any{"plugin": "gh"})
	s.wait("the open list shows the pinned commit B", func() bool { return strings.Contains(detail(), "pinned "+second[:12]) })
	if after, _ := p.renders(); after <= before {
		t.Fatalf("render attempts %d -> %d: the list did not render again", before, after)
	}
}

// The plugin-sync confirm (lead-offer4), through two real lab hubs: b syncs the plugins of a
// (plugins_from). b's Plugins… panel is open when a gets a managed GitHub entry: the offer shows
// in the open panel with no action. Review… opens the confirm form, which names the offer; Add
// adds the entry at a's pinned commit, fetched by b. Then a changes the id of another offer from X
// to Y at the same pin: a press of the form that showed X sends no op. Lab only: LAZYREX_LAB=1.
func TestOfferConfirmLab(t *testing.T) {
	needLab(t)
	gitTestEnv(t)
	r := newTestRepo(t)
	commit := r.commit(map[string]string{"lazyrex.json": `{"name":"gh","main":"main.lua"}`, "main.lua": fmt.Sprintf(startFixture, "gh")})
	r2 := newTestRepo(t)
	r2.commit(map[string]string{"lazyrex.json": `{"name":"gh2","main":"main.lua"}`, "main.lua": fmt.Sprintf(startFixture, "gh2")})
	remotes := map[string]*testRepo{"r": r, "r2": r2}
	// Each hub's own fetch reaches the lab repositories through git's url.insteadOf: no network.
	a := startLabWith(t, nil, false, labGitEnv)
	a.labRepos(remotes)
	b := newStartLabHome(t, labGitEnv)
	b.labRepos(remotes)
	aManaged := filepath.Join(a.l.dir, "managed.json")
	writeA := func(list string, sources ...string) {
		t.Helper()
		if err := os.WriteFile(aManaged, []byte(`{"plugins":[`+list+`]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// hub.peer.plugins lists the managed ids of a's loaded entries: a running hub reloads, and
		// its reload installs the new sources.
		if a.hub != "" {
			a.must("hub.plugins.reload", nil)
		}
		a.wait("a installs "+strings.Join(sources, ", "), func() bool {
			lock, err := readLock(lockPath(a.cfg, ""))
			for _, src := range sources {
				if err != nil || lock.Plugins[src] == nil {
					return false
				}
			}
			return true
		})
	}
	a.setConfig("managed_plugins", aManaged)
	writeA("")
	a.setConfig("hub_name", "a")
	a.setConfig("peers", []any{map[string]any{"name": "b", "server": b.l.server}})
	b.setConfig("hub_name", "b")
	b.setConfig("peers", []any{map[string]any{"name": "a", "server": a.l.server, "plugins_from": true}})
	b.setConfig("plugins", []any{map[string]any{"source": b.o.home.dir}})
	a.start()
	b.start()
	b.registered("lazyrex")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	p := openPluginsPanel(t, b, ctx)
	const src = "github:lab/r@main"
	if n := p.node("offered"); n != nil {
		t.Fatalf("an offer before a has one: %+v", n)
	}
	before, _ := p.renders()

	// a gets the entry: at b's next sync the open panel shows the offer by itself.
	writeA(`{"id":"gh","source":"`+src+`"}`, src)
	b.waitLong("the open panel shows the offer", 90*time.Second, func() bool { return strings.Contains(p.detail("offered", src), commit[:12]) })
	if d := p.detail("offered", src); !strings.Contains(d, "Offered by a") || !strings.Contains(d, "pinned by the peer at main") {
		t.Fatalf("the offer row: %q", d)
	}
	if after, _ := p.renders(); after <= before {
		t.Fatalf("render attempts %d -> %d: the panel did not render again", before, after)
	}

	// Review… opens the confirm form. It names the id, the peer, the full commit and the digest.
	if st := p.action("offered", "review", map[string]any{"item_id": src}, "offer-review"); st["status"] != "succeeded" {
		t.Fatalf("review: %v", st)
	}
	f := b.viewAt(ctx, "/forms/plugins/confirm/offer/"+fmt.Sprintf("%x", src))
	var confirm *labViewNode
	f.waitNode("the confirm node", func() bool { confirm = f.node("offer/"); return confirm != nil })
	if !strings.Contains(confirm.Text, "managed:gh from a") || !strings.Contains(confirm.Text, commit) {
		t.Fatalf("the confirm text: %q", confirm.Text)
	}
	if st := f.action(confirm.ID, "add", map[string]any{}, "offer-add"); st["status"] != "succeeded" || st["effects_error"] != nil {
		t.Fatalf("add: %v", st)
	}
	var entry map[string]any
	b.waitLong("b has managed:gh at a's commit", 60*time.Second, func() bool {
		// The form shows its newest op (hub.plugins.ops of its view).
		if n := f.node("progress"); n != nil && (strings.Contains(n.Text, "· failed") || strings.Contains(n.Text, "· conflict")) {
			t.Fatalf("the add op: %s", n.Text)
		}
		entries, _ := b.must("hub.plugins", nil)["entries"].([]any)
		for _, x := range entries {
			if e := x.(map[string]any); e["managed"] == "gh" {
				entry = e
			}
		}
		return entry != nil && entry["pinned_commit"] == commit
	})
	if entry["source"] != src {
		t.Fatalf("the added entry: %v", entry)
	}

	// A second offer, id x. The form shows x; then a renames it y at the same pin. The press of
	// the form that showed x sends no op.
	const src2 = "github:lab/r2@main"
	writeA(`{"id":"gh","source":"`+src+`"},{"id":"x","source":"`+src2+`"}`, src, src2)
	// The confirm form took the panel's slot: open the panel again.
	p = openPluginsPanel(t, b, ctx)
	b.waitLong("the offer x", 90*time.Second, func() bool { return p.detail("offered", src2) != "" })
	if st := p.action("offered", "review", map[string]any{"item_id": src2}, "offer-review-x"); st["status"] != "succeeded" {
		t.Fatalf("review x: %v", st)
	}
	fx := b.viewAt(ctx, "/forms/plugins/confirm/offer/"+fmt.Sprintf("%x", src2))
	var shown *labViewNode
	fx.waitNode("the confirm node of x", func() bool {
		shown = fx.node("offer/")
		return shown != nil && strings.Contains(shown.Text, "managed:x")
	})
	writeA(`{"id":"gh","source":"`+src+`"},{"id":"y","source":"`+src2+`"}`, src, src2)
	b.waitLong("the offer is y", 90*time.Second, func() bool {
		var list struct {
			Offered []peerOffer `json:"offered"`
		}
		raw, _ := json.Marshal(b.must("hub.plugins", nil))
		json.Unmarshal(raw, &list)
		return len(list.Offered) == 1 && list.Offered[0].ID == "y"
	})
	// The home plugin refuses the press itself (conflict); the hub's admission is the second check.
	st := fx.action(shown.ID, "add", map[string]any{}, "offer-add-x")
	if e, _ := json.Marshal(st["error"]); st["status"] != "failed" || !strings.Contains(string(e), "The offered code changed") {
		t.Fatalf("the press of the form that showed x: %v", st)
	}
	if n := fx.node("progress"); n == nil || n.Text != "No operation yet." {
		t.Fatalf("the stale press made an op: %s", fx.raw())
	}
	if _, ok := b.must("hub.plugins", nil)["entries"].([]any); ok {
		for _, x := range b.must("hub.plugins", nil)["entries"].([]any) {
			if e := x.(map[string]any); e["managed"] == "x" || e["managed"] == "y" {
				t.Fatalf("the stale press added %v", e)
			}
		}
	}
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sdkPeerLab is one lab hub of TestPeerSDKLab: a copy of the fixture plugin tests/sdk-peer (an
// app.peer plugin of the real SDK) and the SDK of this checkout. receivers adds one receiver plugin
// per name, with that app.peer "from" (a Lua expression): its table got keeps each envelope.
func sdkPeerLab(t *testing.T, root string, serverArgs []string, receivers map[string]string) *startLab {
	sources := []string{"speer"}
	for name := range receivers {
		sources = append(sources, name)
	}
	s := newServerLab(t, serverArgs, sources...)
	if err := os.MkdirAll(s.src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(s.src, "speer"), os.DirFS(filepath.Join(root, "tests", "sdk-peer"))); err != nil {
		t.Fatal(err)
	}
	for name, from := range receivers {
		dir := filepath.Join(s.src, name)
		main := "local app = require('lazyrex').plugin('" + name + "')\n" +
			"app.db.migrate {'CREATE TABLE got (from_plugin TEXT, key TEXT, text TEXT)'}\n" +
			"app.peer{from = " + from + ", receive = function(msg)\n" +
			"  app.db.exec('INSERT INTO got (from_plugin, key, text) VALUES (?, ?, ?)', msg.from_plugin, msg.key, msg.body.t)\n" +
			"end}\n"
		manifest := `{"name": "` + name + `", "main": "main.lua", "sdk": "0"}`
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.lua"), []byte(main), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "lazyrex.json"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s.args = append(s.args, "-sdk", filepath.Join(root, "sdk", "lua"))
	return s
}

// kill kills the hub (SIGKILL) and waits until it released hub.lock. Its plugins stay in their
// blocks.
func (s *startLab) kill() {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := stopHubForRestart(ctx, s.l, "kill"); err != nil {
		s.t.Fatalf("kill the hub: %v", err)
	}
}

// speerSQL runs a read on the speer database and returns the first column of each row as text.
func (s *startLab) speerSQL(sql string, params ...any) []string {
	s.t.Helper()
	return s.pluginSQL("speer", sql, params...)
}

// pluginSQL runs a read on the database of plugin and returns the first column of each row as text.
func (s *startLab) pluginSQL(plugin, sql string, params ...any) []string {
	s.t.Helper()
	if params == nil {
		params = []any{}
	}
	r := s.must("hub.test.sql", map[string]any{"plugin": plugin, "sql": sql, "params": params})
	var out []string
	rows, _ := r["rows"].([]any)
	for _, row := range rows {
		v := row.([]any)[0]
		if m, ok := v.(map[string]any); ok {
			v = m["int"]
		}
		out = append(out, fmt.Sprint(v))
	}
	return out
}

// sdkText is the SDK's body text for {t = s} (peer.lua encode_string): it escapes " and \, \b \f
// \n \r \t, and \u00XX for the other bytes below 0x20; < > & and U+2028 stay as they are.
func sdkText(s string) string {
	var b strings.Builder
	b.WriteString(`{"t":"`)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\b', '\f', '\n', '\r', '\t':
			b.WriteString(map[byte]string{'\b': `\b`, '\f': `\f`, '\n': `\n`, '\r': `\r`, '\t': `\t`}[c])
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteString(`"}`)
	return b.String()
}

// sdkFit is the longest text of unit (then 'a' bytes) that the SDK accepts with key k to hub: the
// row insert request rule #enc(text) + 2 #enc(hub) + 2 #enc(key) + 1024 <= 65536, where text is
// sdkText(s). Go's json.Marshal of a string escapes as Rex does (docs/plugin-sdk.md "Send").
func sdkFit(t *testing.T, unit, hub, k string) string {
	enc := func(s string) int { b, _ := json.Marshal(s); return len(b) }
	cost := func(s string) int { return enc(sdkText(s)) + 2*enc(hub) + 2*enc(k) + 1024 }
	base := cost("")
	s := strings.Repeat(unit, (65536-base)/(cost(unit)-base))
	s += strings.Repeat("a", 65536-cost(s))
	if cost(s) != 65536 {
		t.Fatalf("sdkFit %q: cost %d", unit, cost(s))
	}
	return s
}

// TestPeerSDKLab: the SDK federation helper (app.peer) on two lab hubs a and b, end to end (a's
// plugin -> hub a -> hub b -> b's listener), with the real SDK and SQL:
//   - A body at the SDK size limit (quotes, <>&, newlines, U+2028) reaches b's listener whole; one
//     byte more is too_large at the call.
//   - A send to another plugin (lead-fedxp): b's sfrom (from = {"speer"}) commits it once, with
//     from_plugin speer; b's sother (the default from: only itself) refuses it not_allowed, and
//     stores the refusal.
//   - Hub a dies while a row is queued (between the send and the drain): after a's restart the row
//     goes, and b's listener commits it once.
//   - Hub a dies after it took a row and before b pulled it: after both start, b's listener commits
//     it once.
//
// Then both outboxes are empty. b's lab server listens on TCP, as in TestPeerHubsLab.
func TestPeerSDKLab(t *testing.T) {
	needLab(t)
	_, root := harnessBinary(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp := "http://" + ln.Addr().String()
	ln.Close()
	a := sdkPeerLab(t, root, nil, nil)
	b := sdkPeerLab(t, root, []string{"--extra-listen", tcp, "--allow-anonymous"}, map[string]string{"sfrom": `{'speer'}`, "sother": "nil"})
	a.setPeer("a", "b", tcp)
	b.setPeer("b", "a", a.l.server)
	a.start()
	b.start()
	a.registered("speer")
	b.registered("speer")
	b.registered("sfrom")
	b.registered("sother")
	x := a.opsAgent("x")
	send := func(k, text string) (map[string]any, error) {
		return a.agentDo(x, "speer.send", map[string]any{"hub": "b", "k": k, "text": text})
	}
	got := func(k string) []string { return b.speerSQL("SELECT text FROM got WHERE key = ?", k) }
	once := func(k, want string) {
		t.Helper()
		// After a kill, the dead hub's Rex client can keep the label lazyrex-hub for a while: a peer's
		// pull is then "ambiguous" and backs off (up to 30 s).
		t0 := time.Now()
		b.waitLong("b's listener got "+k, 120*time.Second, func() bool { return len(got(k)) > 0 })
		t.Logf("%s reached b's listener in %s", k, time.Since(t0).Round(time.Millisecond))
		time.Sleep(3 * time.Second) // more than one pull interval: a second delivery would show
		if g := got(k); len(g) != 1 || g[0] != want {
			t.Fatalf("b's listener rows for %s: %d rows, want one row with the exact text (%d bytes)", k, len(g), len(want))
		}
	}

	for i, unit := range []string{`"\`, "<>&", "a\nb\r\t", " x"} {
		k := fmt.Sprintf("size-%d", i+1)
		s := sdkFit(t, unit, "b", k)
		if r, err := send(k, s); err != nil || r["state"] != "queued" {
			t.Fatalf("send %s at the limit (%d bytes): %v, %v", k, len(s), r, err)
		}
		if _, err := send(k, s+"a"); err == nil || !strings.Contains(err.Error(), "too_large") {
			t.Fatalf("send %s one byte over the limit: %v, want too_large", k, err)
		}
		once(k, s)
		t.Logf("%s: %d bytes of text at the SDK limit reached b's listener", k, len(s))
	}

	// To another plugin: one commit where "from" names speer; a stored not_allowed where it does not.
	for _, to := range []string{"sfrom", "sother"} {
		if r, err := a.agentDo(x, "speer.send", map[string]any{"hub": "b", "k": "xp-" + to, "text": "to " + to, "plugin": to}); err != nil || r["state"] != "queued" {
			t.Fatalf("send xp-%s: %v, %v", to, r, err)
		}
	}
	b.waitLong("sfrom got xp-sfrom", 120*time.Second, func() bool {
		return len(b.pluginSQL("sfrom", "SELECT key FROM got WHERE key = ?", "xp-sfrom")) > 0
	})
	b.waitLong("sother stored its refusal", 120*time.Second, func() bool {
		return len(b.pluginSQL("sother", "SELECT refused FROM sdk_peer_seen WHERE key = ?", "xp-sother")) > 0
	})
	time.Sleep(3 * time.Second) // more than one pull interval: a second delivery would show
	if g := b.pluginSQL("sfrom", "SELECT from_plugin || ' ' || text FROM got WHERE key = ?", "xp-sfrom"); len(g) != 1 || g[0] != "speer to sfrom" {
		t.Fatalf("sfrom rows for xp-sfrom: %v, want one row \"speer to sfrom\"", g)
	}
	if g := b.pluginSQL("sother", "SELECT refused || ' ' || from_plugin FROM sdk_peer_seen WHERE key = ?", "xp-sother"); len(g) != 1 || g[0] != "not_allowed speer" {
		t.Fatalf("sother seen rows for xp-sother: %v, want one not_allowed row from speer", g)
	}
	if g := b.pluginSQL("sother", "SELECT count(*) FROM got"); len(g) != 1 || g[0] != "0" {
		t.Fatalf("sother listener rows: %v, want none", g)
	}
	if g := b.speerSQL("SELECT count(*) FROM got WHERE key LIKE ?", "xp-%"); len(g) != 1 || g[0] != "0" {
		t.Fatalf("b's speer got an envelope of another target: %v", g)
	}

	// Between the send and the drain: a gate keeps the row queued (a passing error), then hub a dies.
	gate := a.must("hub.test.fault", map[string]any{"plugin": "speer", "point": "action", "action": "hub.peer.send", "when": "before", "mode": "gate"})["id"]
	if _, err := send("kill-queued", "q"); err != nil {
		t.Fatal(err)
	}
	a.wait("the gate", func() bool {
		return a.must("hub.test.fault_state", map[string]any{"id": gate})["fired"] == true
	})
	if st := a.speerSQL("SELECT state FROM sdk_peer_outbox WHERE key = ?", "kill-queued"); len(st) != 1 || st[0] != "queued" {
		t.Fatalf("kill-queued before the kill: %v", st)
	}
	a.kill()
	a.start()
	a.registered("speer")
	once("kill-queued", "q")

	// Between the take and the pull: b is down while a takes the row; then a dies.
	b.stop()
	if _, err := send("kill-taken", "t"); err != nil {
		t.Fatal(err)
	}
	a.wait("a took kill-taken", func() bool {
		st := a.speerSQL("SELECT state FROM sdk_peer_outbox WHERE key = ?", "kill-taken")
		return len(st) == 1 && st[0] == "sent"
	})
	a.kill()
	a.start()
	b.start()
	a.registered("speer")
	b.registered("speer")
	once("kill-taken", "t")

	for _, s := range []*startLab{a, b} {
		s.waitLong("an empty outbox and an up link", 120*time.Second, func() bool {
			st := s.must("hub.peer.status", map[string]any{})
			p := st["peers"].([]any)[0].(map[string]any)
			return p["state"] == "up" && p["outbox"] == float64(0)
		})
		if q := s.speerSQL("SELECT count(*) FROM sdk_peer_outbox WHERE state = ?", "queued"); len(q) != 1 || q[0] != "0" {
			t.Fatalf("queued SDK rows: %v", q)
		}
	}
}

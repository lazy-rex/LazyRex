package main

// Home-session tabs: a plugin tab may name the Rex session it belongs in
// (hub.ui.tabs entry session_id), for example a message thread's home session, where the thread
// was created. The hub then opens, titles and closes that tab in that session, next to the user's
// own tabs, and never reorders them. A tab without session_id stays in the LazyRex session.
//
// A home session that is gone: when one of its tabs must show, the hub uses a
// live session with the home's name, or creates one with that name, and the tab lives there from
// then on (ui_homes.moved_to). The hub learns each home's name while the session lives. A home
// whose name it never learned sends its tabs to the app session. A closed tab in a gone home
// creates nothing; a new revision makes it show, and then the session comes back.
//
// Reopen: a tab entry may carry revision. A manual close records the tab's revision; a later
// declaration with another revision opens the tab again, without focus (the maintainer: a new message
// reopens a closed thread tab). A navigation always opens it. A tab closed before revisions existed
// (ui_tabs.rev_known 0) takes the first declared revision as its baseline and stays closed: only a
// later change reopens it. A tab closed with an empty revision (an empty thread) reopens on its
// first nonempty revision.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

const homeSchema = `
-- Home sessions of plugin tabs (hometabs.go): the name last seen, and the session that replaced
-- the home after it was gone.
CREATE TABLE IF NOT EXISTS ui_homes (session_id TEXT PRIMARY KEY, label TEXT NOT NULL DEFAULT '', moved_to TEXT NOT NULL DEFAULT '', label_at TEXT NOT NULL DEFAULT '');
-- Session names (sessionRenamed): the label the hub last wrote to one of its sessions, and a label
-- the user gave it. A user label stops the hub from writing its own.
CREATE TABLE IF NOT EXISTS ui_session_names (session_id TEXT PRIMARY KEY, hub_label TEXT NOT NULL DEFAULT '', user_label TEXT NOT NULL DEFAULT '');
`

type tabGroup struct {
	sess  string
	tabs  []tab
	views []*view
}

// tabGroups splits a plugin's tabs and views by target session. The app session (appSess, ""
// when it does not exist) comes first and also gets the views of any session that no tab targets:
// those retire there (the legacy path). Without an app session they wait.
func (h *Hub) tabGroups(ctx context.Context, appSess string, tabs []tab, views []*view) ([]tabGroup, error) {
	order := []string{appSess}
	idx := map[string]int{appSess: 0}
	target := map[string]string{}   // tab key -> session
	resolved := map[[2]any]string{} // (home, need) -> session; one lookup per pass
	var list *sessionList
	for _, t := range tabs {
		s := appSess
		if t.Session != "" {
			key := [2]any{t.Session, !t.Suppressed}
			home, seen := resolved[key]
			if !seen {
				var err error
				if home, err = h.resolveHome(ctx, t.Session, !t.Suppressed, &list); err != nil {
					return nil, err // unknown: no tab moves on an uncertain answer
				}
				resolved[key] = home
			}
			if home != "" {
				s = home
			}
		}
		target[t.Key] = s
		if _, ok := idx[s]; !ok {
			idx[s] = len(order)
			order = append(order, s)
		}
	}
	groups := make([]tabGroup, len(order))
	for i, s := range order {
		groups[i].sess = s
	}
	for _, t := range tabs {
		g := &groups[idx[target[t.Key]]]
		g.tabs = append(g.tabs, t)
	}
	for _, v := range views {
		i, ok := idx[v.Session]
		if !ok || v.Session == "" {
			i = 0 // a view in no target session: retires in the app session's pass
		}
		groups[i].views = append(groups[i].views, v)
	}
	out := groups[:0]
	for _, g := range groups {
		if g.sess != "" {
			out = append(out, g)
		}
	}
	return out, nil
}

type sessionList struct {
	fetched  time.Time // when the hub asked; an older answer never overwrites a newer event
	Sessions []struct {
		SessionID string `json:"session_id"`
		Label     string `json:"label"`
	} `json:"sessions"`
}

func (h *Hub) sessions(ctx context.Context, list **sessionList) (*sessionList, error) {
	if *list == nil {
		var l sessionList
		l.fetched = time.Now()
		if err := h.rex.Call(ctx, "session.list", map[string]any{}, &l); err != nil {
			return nil, err
		}
		*list = &l
	}
	return *list, nil
}

// resolveHome returns the session where tabs of home sess go now: the home itself while it
// lives, the session that replaced it, or "" (the app session). need: a tab must show, so a gone
// home is replaced (a live session with its name, or a new one). Only a definite "session not
// found" counts as gone; any other error is returned.
func (h *Hub) resolveHome(ctx context.Context, sess string, need bool, list **sessionList) (string, error) {
	if !strings.HasPrefix(sess, "session:") {
		return "", nil
	}
	cur, err := h.followHome(ctx, sess)
	if err != nil {
		return "", err
	}
	if !rexSessionID(cur) {
		// Never a session: Rex refuses the attach with code internal, not "not found", and that
		// error failed every reconcile pass. The app session, reported once.
		if !h.ui.badHomes[cur] {
			if h.ui.badHomes == nil {
				h.ui.badHomes = map[string]bool{}
			}
			h.ui.badHomes[cur] = true
			log.Printf("ui: home %s is not a Rex session ID; its tabs use the app session", cur)
			h.ev.emit("ui_home_invalid", map[string]any{"home": sess, "session_id": cur})
		}
		return "", nil
	}
	alive, err := h.homeAlive(ctx, cur)
	if err != nil {
		return "", err
	}
	l, err := h.sessions(ctx, list)
	if err != nil {
		return "", err
	}
	if alive {
		for _, s := range l.Sessions {
			if s.SessionID == cur && s.Label != "" {
				// Remember the name while the session lives: it names the replacement.
				if err := h.rememberHomeLabel(ctx, cur, s.Label, l.fetched, false); err != nil {
					return "", err
				}
			}
		}
		return cur, nil
	}
	if !need {
		return "", nil
	}
	var label string
	if err := h.db.QueryRowContext(ctx, `SELECT label FROM ui_homes WHERE session_id = ?`, cur).Scan(&label); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if label == "" {
		return "", nil // name never learned: the app session
	}
	repl := ""
	for _, s := range l.Sessions {
		if s.Label == label && s.SessionID != cur {
			repl = s.SessionID // the user (or an earlier pass) made it again
			break
		}
	}
	if repl == "" {
		var out struct {
			SessionID string `json:"session_id"`
		}
		if err := h.rex.Call(ctx, "session.create", map[string]any{"label": label}, &out); err != nil {
			return "", err
		}
		repl = out.SessionID
		*list = nil // the list changed
	}
	if ok, err := h.homeAlive(ctx, repl); err != nil || !ok {
		return "", err
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO ui_homes (session_id, label, moved_to) VALUES (?, ?, ?)
		ON CONFLICT (session_id) DO UPDATE SET moved_to = excluded.moved_to`, cur, label, repl); err != nil {
		return "", err
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO ui_homes (session_id, label) VALUES (?, ?)
		ON CONFLICT (session_id) DO NOTHING`, repl, label); err != nil {
		return "", err
	}
	h.ev.emit("home_replaced", map[string]any{"home": sess, "was": cur, "session_id": repl, "label": label})
	return repl, nil
}

// followHome follows the replacements of home sess (ui_homes.moved_to) to the last one. A read
// error is returned; a cycle (never written by the hub) is an error too.
func (h *Hub) followHome(ctx context.Context, sess string) (string, error) {
	seen := map[string]bool{}
	for cur := sess; ; {
		if seen[cur] {
			return "", fmt.Errorf("home %s: replacement cycle at %s", sess, cur)
		}
		seen[cur] = true
		var next string
		err := h.db.QueryRowContext(ctx, `SELECT moved_to FROM ui_homes WHERE session_id = ?`, cur).Scan(&next)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if next == "" {
			return cur, nil
		}
		cur = next
	}
}

// homeRedirects maps each replaced home to the session its tabs use now, the last replacement.
func (h *Hub) homeRedirects(ctx context.Context) (map[string]string, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT session_id FROM ui_homes WHERE moved_to != ''`)
	if err != nil {
		return nil, err
	}
	var homes []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return nil, err
		}
		homes = append(homes, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := map[string]string{}
	for _, a := range homes {
		cur, err := h.followHome(ctx, a)
		if err != nil {
			return nil, err
		}
		out[a] = cur
	}
	return out, nil
}

// rexSessionID reports whether id has the form of a Rex session ID: "session:" and a UUID
// (8-4-4-4-12 hex digits). Observed on Rex 0.0.1792158932-g88fd41: session.attach of
// "session:kv-home" fails with status 500, code internal ("invalid session ID UUID").
func rexSessionID(id string) bool {
	u, ok := strings.CutPrefix(id, "session:")
	if !ok || len(u) != 36 {
		return false
	}
	for i, c := range u {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'):
			return false
		}
	}
	return true
}

// homeAlive attaches the hub to a home session (its events then reach the hub) and reports
// whether the session exists. Only a definite "session not found" is absence; any other error is
// returned, so a timeout or a lost connection never moves a live home's tabs.
func (h *Hub) homeAlive(ctx context.Context, sess string) (bool, error) {
	err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil)
	switch {
	case err == nil:
		return true, nil
	case sessionGone(err):
		return false, nil
	}
	return false, err
}

// homeOf reports whether sess is where the plugin's tab lives: its declared home session, or
// the session that replaced that home. A view there is not a legacy view. The callers treat an
// error as "home": an uncertain answer never retires a view as legacy.
func (h *Hub) homeOf(ctx context.Context, plugin, tabKey, sess string) (bool, error) {
	var home string
	err := h.db.QueryRowContext(ctx, `SELECT session_id FROM ui_tabs WHERE plugin = ? AND tab = ?`, plugin, tabKey).Scan(&home)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && home == "") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if home == sess {
		return true, nil
	}
	cur, err := h.followHome(ctx, home)
	return cur == sess, err
}

// notHome reports whether a view in sess is definitely not in the tab's home (see homeOf).
func (h *Hub) notHome(ctx context.Context, plugin, tabKey, sess string) bool {
	home, err := h.homeOf(ctx, plugin, tabKey, sess)
	if err != nil {
		log.Printf("home of %s/%s: %v", plugin, tabKey, err)
		return false
	}
	return !home
}

// rememberHomeLabel stores label as the name of home sess, seen at time at. A name seen earlier
// than the stored one does not replace it: a session.list answer that was asked for before a
// rename event is older than that event (Codex review). On equal times (the clock can give the same
// reading twice) an event wins over a list answer, and a later event wins over an earlier one.
func (h *Hub) rememberHomeLabel(ctx context.Context, sess, label string, at time.Time, event bool) error {
	cmp := "<"
	if event {
		cmp = "<="
	}
	_, err := h.db.ExecContext(ctx, `INSERT INTO ui_homes (session_id, label, label_at) VALUES (?, ?, ?)
		ON CONFLICT (session_id) DO UPDATE SET label = excluded.label, label_at = excluded.label_at
		WHERE ui_homes.label_at `+cmp+` excluded.label_at`, sess, label, at.UTC().Format(labelTime))
	return err
}

// labelTime sorts as text in time order.
const labelTime = "2006-01-02T15:04:05.000000000Z"

// sessionRenamed handles session_label_changed (the maintainer: sessions can be renamed). The event loop
// calls it in event order, so rapid renames are stored in the order Rex made them.
//   - A home keeps its new name at once, not only on the next reconcile pass: a rename
//     followed by a close brings the session back with the new name.
//   - A label on a hub session that is not the label the hub last wrote is the user's. The hub
//     then keeps it, also after a restart (appSession).
func (h *Hub) sessionRenamed(ctx context.Context, sess, label string) {
	if label == "" {
		return
	}
	// Any session may be or become a home: store the name even before the home's first lookup, so
	// that lookup's older session.list answer cannot win (Codex review).
	if err := h.rememberHomeLabel(ctx, sess, label, time.Now(), true); err != nil {
		log.Printf("home %s renamed: %v", sess, err)
	}
	h.ui.mu.Lock()
	_, ours := h.ui.sess[sess]
	h.ui.mu.Unlock()
	if !ours {
		return
	}
	var hubLabel string
	err := h.db.QueryRowContext(ctx, `SELECT hub_label FROM ui_session_names WHERE session_id = ?`, sess).Scan(&hubLabel)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("session %s renamed: %v", sess, err) // no reliable provenance: classify nothing
		return
	}
	if label == hubLabel {
		return // the echo of the hub's own write
	}
	if err := h.setUserLabel(ctx, sess, label); err != nil {
		log.Printf("session %s renamed: %v", sess, err)
		return
	}
	h.ev.emit("session_renamed", map[string]any{"session_id": sess, "label": label})
}

func (h *Hub) setUserLabel(ctx context.Context, sess, label string) error {
	_, err := h.db.ExecContext(ctx, `INSERT INTO ui_session_names (session_id, user_label) VALUES (?, ?)
		ON CONFLICT (session_id) DO UPDATE SET user_label = excluded.user_label`, sess, label)
	return err
}

// mayLabel reports whether the hub may write label to its session sess, and records the label as
// the hub's own first. It returns false when the user named the session: by an event, or while the
// hub was stopped (the current label is neither the hub's last label nor the wanted one), or before
// any provenance existed (a label other than the wanted one). A read or write error is returned:
// without reliable provenance the hub writes nothing now and decides again later (Codex review).
func (h *Hub) mayLabel(ctx context.Context, sess, label string) (bool, error) {
	var hubLabel, userLabel string
	err := h.db.QueryRowContext(ctx, `SELECT hub_label, user_label FROM ui_session_names WHERE session_id = ?`, sess).Scan(&hubLabel, &userLabel)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if userLabel != "" {
		return false, nil
	}
	var l sessionList
	if err := h.rex.Call(ctx, "session.list", map[string]any{}, &l); err != nil {
		return false, err
	}
	current := ""
	for _, x := range l.Sessions {
		if x.SessionID == sess {
			current = x.Label
		}
	}
	if current != "" && current != label && current != hubLabel {
		// Renamed while the hub was stopped, or before the hub recorded its labels: keep it.
		if err := h.setUserLabel(ctx, sess, current); err != nil {
			return false, err
		}
		h.ev.emit("session_renamed", map[string]any{"session_id": sess, "label": current, "while": "hub stopped"})
		return false, nil
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO ui_session_names (session_id, hub_label) VALUES (?, ?)
		ON CONFLICT (session_id) DO UPDATE SET hub_label = excluded.hub_label`, sess, label); err != nil {
		return false, err
	}
	return current != label, nil
}

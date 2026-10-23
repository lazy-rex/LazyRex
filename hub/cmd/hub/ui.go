package main

// UI framework, hub side (../../README.md and docs/architecture.md, "UI framework"):
// the LazyRex session, declared tabs, views, the reconciler, renderer check-in, and navigation.
//
// Every plugin's tabs are mounted in the one LazyRex session. The hub owns a session by its record
// in ui_sessions, never by its label. App mode and per-plugin app sessions are removed
// (docs/hub.md, "Removed: app mode").
//
// Rex delivers requests to the hub one at a time (at most 16 waiting), so handlers here only
// record intent and return. One worker does the Rex work for the whole session, serially.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

const uiSchema = `
-- plugin '*' is the LazyRex session, which every plugin shares. It is the only row.
CREATE TABLE IF NOT EXISTS ui_sessions (plugin TEXT PRIMARY KEY, session_id TEXT NOT NULL);
-- key 'nav': the latest navigation target "<plugin>/<tab>", done when shown. (key 'active' is from
-- the single-session build and is not read.)
CREATE TABLE IF NOT EXISTS ui_state (key TEXT PRIMARY KEY, value TEXT NOT NULL);
-- A restore in progress: its placeholder pane carries the nonce. Recorded before the pane exists.
CREATE TABLE IF NOT EXISTS ui_restores (nonce TEXT PRIMARY KEY, view_id TEXT NOT NULL, created TEXT NOT NULL);
-- The tab set each plugin declared. suppressed: the user closed the tab; it stays closed until a navigation.
CREATE TABLE IF NOT EXISTS ui_tabs (plugin TEXT NOT NULL, tab TEXT NOT NULL, title TEXT NOT NULL, route TEXT NOT NULL,
  pos INTEGER NOT NULL, suppressed INTEGER NOT NULL DEFAULT 0, crashes INTEGER NOT NULL DEFAULT 0,
  session_id TEXT NOT NULL DEFAULT '', revision TEXT NOT NULL DEFAULT '', closed_revision TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '', PRIMARY KEY (plugin, tab));
-- status: prepared (request not sent), sending (outcome unknown), open, exited (renderer ended; the worker closes it), closed
CREATE TABLE IF NOT EXISTS views (view_id TEXT PRIMARY KEY, plugin TEXT NOT NULL, tab TEXT NOT NULL, route TEXT NOT NULL,
  status TEXT NOT NULL, session_id TEXT NOT NULL DEFAULT '', window_id TEXT NOT NULL DEFAULT '', block_id TEXT NOT NULL DEFAULT '',
  child_pid INTEGER NOT NULL DEFAULT 0, child_start TEXT NOT NULL DEFAULT '', client_id TEXT NOT NULL DEFAULT '',
  attachment TEXT NOT NULL DEFAULT '', closed_by TEXT NOT NULL DEFAULT '',
  attach_state TEXT NOT NULL DEFAULT 'unknown', open_args TEXT NOT NULL DEFAULT '');
-- attach_state (panels.go): never (a new view, no renderer yet), attached, or claimed (a replacement
-- took the never-attached view). Every insert names it; 'unknown' counts as attached.
-- open_args (reopen.go): the args of the reopen that made the view, JSON, or '' for none.
`

var errRetry = errors.New("retry")

const (
	maxSpawnsPerPass = 4 // bounds a spawn storm; the worker runs again for the rest
	maxCrashes       = 3 // renderer crashes before the hub stops respawning a tab
)

const (
	uiSessionKey   = "*"
	uiSessionLabel = "LazyRex"
)

// sessionGone reports whether err says the session does not exist. Rex 1203 answers a missing
// session with code session_not_found, and a missing block or window with not_found.
func sessionGone(err error) bool {
	var re *rex.RexError
	return errors.As(err, &re) && (re.Code == "session_not_found" || re.Code == "not_found")
}

type uiState struct {
	mu         sync.Mutex
	kick       chan struct{}
	spawned    int                       // views opened in this pass (worker only)
	retryArmed bool                      // a delayed kick is scheduled (retryLater)
	badHomes   map[string]bool           // tab homes reported as not a Rex session ID (worker only)
	labeled    map[string]string         // session_id -> the label the worker last set
	sess       map[string]string         // session_id -> ui_sessions key, for event routing
	recorded   chan struct{}             // closed (and replaced) when a session is recorded (sessionRecorded)
	expect     map[string]map[string]int // window -> labels the hub wrote whose echo is due (tabrename.go)
	renaming   map[string]pendingRename  // window -> a user rename waiting for its plugin
	labelQ     []labelEvent              // window_label_changed events waiting for the label worker, in order
	labelKick  chan struct{}             // wakes the label worker
	navMu      sync.Mutex                // serializes the navigation order check and its commit (uiNavigate)
	navSeq     int64                     // the newest navigation request
	navOpens   map[string]*navOpen       // navigation_id -> a view.open that runs outside the request queue
	// own: the views that this hub process inserted (spawnView, spawnTabPane). Only their renderer
	// found gone by a pass counts a crash: an earlier process's dead renderer counts none (mu).
	own map[string]bool
}

// navOpen is one hub.ui.navigate to an undeclared tab. view.open runs in the background, because
// the plugin's own calls to the hub (hub.ui.tabs) wait in the hub's request queue
// (rex-protocol.md, "The hub has one request queue").
type navOpen struct {
	seq     int64
	started time.Time
	done    bool
	out     map[string]any
	err     error
}

// sessionRecorded returns a channel that is closed when the hub records its next session.
func (h *Hub) sessionRecorded() <-chan struct{} {
	h.ui.mu.Lock()
	defer h.ui.mu.Unlock()
	if h.ui.recorded == nil {
		h.ui.recorded = make(chan struct{})
	}
	return h.ui.recorded
}

type view struct {
	ID, Plugin, Tab, Route, Status, Session, Window, Block string
	ChildPID                                               int
	ChildStart, ClientID, Attachment                       string
}

type tab struct {
	Key, Title, Route string
	Pos, Crashes      int
	Suppressed        bool
	Session           string // the home session (hometabs.go); empty: the plugin's app session
	Revision          string // reopens a closed tab when it changes (hometabs.go)
	Status            string // declared tab status, JSON (tabstatus.go); "" = none
}

func randomID(prefix string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// uiStart creates the tables, attaches the sessions and starts the worker.
func (h *Hub) uiStart(ctx context.Context) error {
	h.ui = &uiState{kick: make(chan struct{}, 1), sess: map[string]string{}, labeled: map[string]string{}, own: map[string]bool{}}
	if _, err := h.db.ExecContext(ctx, uiSchema); err != nil {
		return err
	}
	if err := formsStart(ctx, h.db); err != nil {
		return err
	}
	if _, err := h.db.ExecContext(ctx, ensuredSchema); err != nil {
		return err
	}
	if _, err := h.db.ExecContext(ctx, panelSchema); err != nil {
		return err
	}
	if err := h.slotStart(ctx); err != nil {
		return err
	}
	if _, err := h.db.ExecContext(ctx, homeSchema); err != nil {
		return err
	}
	// Attachments belong to one hub generation (Renderer contract). Renderers attach again.
	if _, err := h.db.ExecContext(ctx, `UPDATE views SET client_id = '', attachment = '' WHERE status = 'open'`); err != nil {
		return err
	}
	rows, err := h.db.QueryContext(ctx, `SELECT plugin, session_id FROM ui_sessions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var plugin, sess string
		if err := rows.Scan(&plugin, &sess); err != nil {
			return err
		}
		h.ui.sess[sess] = plugin
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for sess, plugin := range h.ui.sess {
		// Every recorded session: its close events close its views.
		if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
			log.Printf("ui: session %s (%s): %v", sess, plugin, err)
		}
	}
	// The LazyRex session is also the work session: it must exist before identities, plugins and
	// workers are read. A session the user closed is created again here.
	_, err = h.appSession(ctx, uiSessionKey, uiSessionLabel)
	return err
}

// workSession is the LazyRex session (ui_sessions key "*"). Agents, plugin processes and workers
// run there. The hub owns it, so closing a user session never stops them. It is "" while the
// session is gone; the next UI pass creates it again.
func (h *Hub) workSession() string {
	h.ui.mu.Lock()
	defer h.ui.mu.Unlock()
	for sess, key := range h.ui.sess {
		if key == uiSessionKey {
			return sess
		}
	}
	return ""
}

// homePlugin is the plugin whose pass runs first (config "home", default "lazyrex"). It declares
// no tabs (uiTabs).
func (h *Hub) homePlugin() string {
	if h.cfg.Home != "" {
		return h.cfg.Home
	}
	return "lazyrex"
}

// uiNav reads the navigation target.
func (h *Hub) uiNav(ctx context.Context) (string, error) {
	var v string
	err := h.db.QueryRowContext(ctx, `SELECT value FROM ui_state WHERE key = 'nav'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// kickUI asks the worker to reconcile. Repeated kicks coalesce. The plugin is for callers'
// readability only: one pass covers every plugin.
func (h *Hub) kickUI(plugin string) {
	select {
	case h.ui.kick <- struct{}{}:
	default:
	}
}

func (h *Hub) uiWorker(ctx context.Context) {
	for range h.ui.kick {
		again, err := h.reconcile(ctx)
		if err != nil {
			log.Printf("ui: %v", err)
			h.ev.emit("ui_reconcile_failed", map[string]any{"error": err.Error()})
			time.Sleep(time.Second)
			again = true
		}
		if again {
			h.kickUI("")
		}
	}
}

func (h *Hub) loadViews(ctx context.Context, plugin string) ([]*view, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT view_id, tab, route, status, session_id, window_id, block_id, child_pid, child_start,
		client_id, attachment FROM views WHERE plugin = ? AND status != 'closed' AND tab != '`+formTab+`'`, plugin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*view
	for rows.Next() {
		v := &view{Plugin: plugin}
		if err := rows.Scan(&v.ID, &v.Tab, &v.Route, &v.Status, &v.Session, &v.Window, &v.Block, &v.ChildPID, &v.ChildStart,
			&v.ClientID, &v.Attachment); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (h *Hub) loadTabs(ctx context.Context, plugin string) ([]tab, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT tab, title, route, pos, suppressed, crashes, session_id, revision FROM ui_tabs WHERE plugin = ? ORDER BY pos`, plugin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tab
	for rows.Next() {
		var t tab
		if err := rows.Scan(&t.Key, &t.Title, &t.Route, &t.Pos, &t.Suppressed, &t.Crashes, &t.Session, &t.Revision); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// markOwn records a view that this hub process inserted (uiState.own).
func (h *Hub) markOwn(id string) {
	h.ui.mu.Lock()
	h.ui.own[id] = true
	h.ui.mu.Unlock()
}

func (h *Hub) ownView(id string) bool {
	h.ui.mu.Lock()
	defer h.ui.mu.Unlock()
	return h.ui.own[id]
}

// forgetOwn deletes a closed view from uiState.own. Every path that closes a view row calls it.
func (h *Hub) forgetOwn(id string) {
	h.ui.mu.Lock()
	delete(h.ui.own, id)
	h.ui.mu.Unlock()
}

// closeViewRow closes a view's row.
func (h *Hub) closeViewRow(ctx context.Context, id, by string) {
	h.forgetOwn(id)
	h.forgetView(id)
	h.db.ExecContext(ctx, `UPDATE views SET status = 'closed', closed_by = ?, client_id = '', attachment = '' WHERE view_id = ?`, by, id)
}

// closeViewOnce closes a view row that is not closed yet, and reports whether this call closed
// it. A renderer's exit, its pane's close and a close from the view or the hub can arrive
// together: only the call that closes the row reports the close and starts its effects.
// keepAttach keeps the client and attachment, for viewClose's check of a lost answer.
func (h *Hub) closeViewOnce(ctx context.Context, id, by string, keepAttach bool) (bool, error) {
	q := `UPDATE views SET status = 'closed', closed_by = ?, client_id = '', attachment = '' WHERE view_id = ? AND status != 'closed'`
	if keepAttach {
		q = `UPDATE views SET status = 'closed', closed_by = ? WHERE view_id = ? AND status != 'closed'`
	}
	res, err := h.db.ExecContext(ctx, q, by, id)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return false, err
	}
	h.forgetOwn(id)
	h.forgetView(id)
	return true, nil
}

// appSession returns the session recorded under key, labeled label. It adopts or creates one.
func (h *Hub) appSession(ctx context.Context, key, label string) (string, error) {
	var sess string
	err := h.db.QueryRowContext(ctx, `SELECT session_id FROM ui_sessions WHERE plugin = ?`, key).Scan(&sess)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if sess != "" {
		err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil)
		if err == nil {
			if h.ui.labeled[sess] != label {
				// A plugin can rename its app. A name the user gave the session wins (mayLabel).
				may, err := h.mayLabel(ctx, sess, label)
				switch {
				case err != nil:
					log.Printf("ui: label %s: %v", sess, err) // undecided: the next pass asks again
				case may:
					if err := h.rex.Call(ctx, "session.set_label", map[string]any{"session_id": sess, "label": label}, nil); err != nil {
						log.Printf("ui: label %s: %v", sess, err)
					} else {
						h.ui.labeled[sess] = label
					}
				default:
					h.ui.labeled[sess] = label // decided: ask again only when the wanted label changes
				}
			}
			return sess, nil
		}
		if !sessionGone(err) {
			return "", err
		}
		h.forgetSession(ctx, sess, "session gone")
		sess = ""
	}
	// Rex create methods have no idempotency key. Adopt one unrecorded session with exactly this
	// label, for example one created just before a hub crash. The label is a hint; the hub owns
	// what it adopts, and it never adopts a session recorded under another key.
	var list struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			Label     string `json:"label"`
		} `json:"sessions"`
	}
	if err := h.rex.Call(ctx, "session.list", map[string]any{}, &list); err != nil {
		return "", err
	}
	h.ui.mu.Lock()
	var found []string
	for _, s := range list.Sessions {
		if _, recorded := h.ui.sess[s.SessionID]; s.Label == label && !recorded {
			found = append(found, s.SessionID)
		}
	}
	h.ui.mu.Unlock()
	if len(found) > 1 {
		// Two candidates: the label cannot say which one is ours. Do not guess.
		h.ev.emit("ui_session_conflict", map[string]any{"sessions": found, "label": label})
		return "", fmt.Errorf("%d sessions are labeled %q; close or rename the extra ones", len(found), label)
	}
	if len(found) == 1 {
		sess = found[0]
	}
	if sess == "" {
		var out struct {
			SessionID string `json:"session_id"`
		}
		if err := h.rex.Call(ctx, "session.create", map[string]any{"label": label}, &out); err != nil {
			return "", err
		}
		sess = out.SessionID
	}
	if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
		return "", err
	}
	if _, err := h.db.ExecContext(ctx, `INSERT OR REPLACE INTO ui_sessions VALUES (?, ?)`, key, sess); err != nil {
		return "", err
	}
	h.ui.mu.Lock()
	h.ui.sess[sess] = key
	h.ui.labeled[sess] = label
	if h.ui.recorded != nil {
		close(h.ui.recorded)
		h.ui.recorded = nil
	}
	h.ui.mu.Unlock()
	h.ev.emit("ui_session", map[string]any{"session_id": sess, "key": key, "label": label})
	return sess, nil
}

// forgetSession records that a session is gone. Its views close; the tabs stay declared.
func (h *Hub) forgetSession(ctx context.Context, sess, reason string) {
	h.db.ExecContext(ctx, `DELETE FROM ui_sessions WHERE session_id = ?`, sess)
	var ids []string
	if rows, err := h.db.QueryContext(ctx, `SELECT view_id FROM views WHERE session_id = ? AND status != 'closed'`, sess); err == nil {
		for rows.Next() {
			var id string
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
	}
	for _, id := range ids {
		h.forgetOwn(id)
		h.forgetView(id)
	}
	h.db.ExecContext(ctx, `UPDATE views SET status = 'closed', closed_by = 'session', client_id = '', attachment = ''
		WHERE session_id = ? AND status != 'closed'`, sess)
	h.ui.mu.Lock()
	delete(h.ui.sess, sess)
	h.ui.mu.Unlock()
	h.ev.emit("ui_session_gone", map[string]any{"session_id": sess, "reason": reason})
}

type rexBlock struct {
	BlockID   string `json:"block_id"`
	Label     string `json:"label"`
	Placement string `json:"placement"`
	WindowID  string `json:"window_id"`
}

func (h *Hub) blocksIn(ctx context.Context, sess string) (map[string]rexBlock, error) {
	var out struct {
		Blocks []rexBlock `json:"blocks"`
	}
	if err := h.rex.Call(ctx, "session.list_blocks", map[string]any{"session_id": sess}, &out); err != nil {
		return nil, err
	}
	m := map[string]rexBlock{}
	for _, b := range out.Blocks {
		m[b.BlockID] = b
	}
	return m, nil
}

func (h *Hub) childIn(ctx context.Context, sess, blockID string) (int, error) {
	var out struct {
		Child *struct {
			PID int `json:"pid"`
		} `json:"child"`
	}
	if err := h.rex.BlockCall(ctx, "com.superlogical.terminal.process", sess, blockID, nil, &out); err != nil {
		return 0, err
	}
	if out.Child == nil {
		return 0, nil
	}
	return out.Child.PID, nil
}

// uiPlugins returns the plugins with declared tabs or a live tab view (the views that loadViews
// reads), the home plugin first. A plugin that declares no tabs keeps its pass while a view of it
// is live, so that view closes and is not stranded.
func (h *Hub) uiPlugins(ctx context.Context) ([]string, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT plugin FROM ui_tabs UNION
		SELECT plugin FROM views WHERE status != 'closed' AND tab != '`+formTab+`' ORDER BY plugin`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	home := h.homePlugin()
	out := []string{}
	hasHome := false
	for rows.Next() {
		var p string
		rows.Scan(&p)
		if p == home {
			hasHome = true
		} else {
			out = append(out, p)
		}
	}
	if hasHome {
		out = append([]string{home}, out...)
	}
	return out, rows.Err()
}

// reconcile makes each plugin's session match its declared tabs, then
// applies the latest navigation. It returns true when more work remains.
func (h *Hub) reconcile(ctx context.Context) (bool, error) {
	h.ui.spawned = 0
	plugins, err := h.uiPlugins(ctx)
	if err != nil {
		return false, err
	}
	// The LazyRex session exists before any plugin declares a tab.
	appSess, err := h.appSession(ctx, uiSessionKey, uiSessionLabel)
	if err != nil {
		return false, err
	}
	more := false
	byKey := map[string]*view{} // mounted views, keys "<plugin>/<tab>"
	for _, plugin := range plugins {
		allViews, err := h.loadViews(ctx, plugin)
		if err != nil {
			return false, err
		}
		allTabs, err := h.loadTabs(ctx, plugin)
		if err != nil {
			return false, err
		}
		groups, err := h.tabGroups(ctx, appSess, allTabs, allViews)
		if err != nil {
			return false, err
		}
		// A reopen (reopen.go) owns its tab while its row is active: this pass opens no view for it and
		// closes neither of its views. The row closes the old view; its end kicks a normal pass.
		reserved := h.reservedTabs(ctx, plugin)
		// A view in a session that is not its tab's target closes (closeMoved) before any group
		// opens a view: its tab opens again in its target session once its pane is closed. The
		// target can be another group, so a failed close blocks the tab in every group. A view's
		// group is its own session while another tab targets that session, so the check also
		// compares the target of the view's own tab.
		target := map[string]string{} // tab key -> its target session
		for _, g := range groups {
			for _, t := range g.tabs {
				target[t.Key] = g.sess
			}
		}
		moved := func(v *view, sess string) bool {
			s, ok := target[v.Tab]
			return v.Session != "" && (v.Session != sess || ok && v.Session != s)
		}
		movedTab := map[string]bool{} // tabs whose moved view's pane is not closed yet: no new view
		for _, g := range groups {
			for _, v := range g.views {
				if moved(v, g.sess) && !h.closeMoved(ctx, v) {
					movedTab[v.Tab] = true
					h.retryLater() // a failed close needs no other event to go on
				}
			}
		}
		for _, g := range groups {
			sess, tabs, views, inApp := g.sess, g.tabs, g.views, g.sess == appSess
			blocks, err := h.blocksIn(ctx, sess)
			if err != nil {
				return false, err
			}
			if inApp {
				if err := h.closePlaceholders(ctx, sess, blocks); err != nil {
					return false, err
				}
			}

			// 1. Resolve spawns with an unknown outcome, and views whose pane is gone.
			byTab := map[string]*view{}
			for _, v := range views {
				if moved(v, sess) {
					continue // a moved view: closed above
				}
				if r := reserved[v.Tab]; r != nil {
					if v.ID == r.New {
						continue // the row adopts or abandons its new view
					}
					if v.ID == r.Old {
						if _, ok := blocks[v.Block]; !ok && v.Status == "open" {
							h.tabReplOldGone(ctx, r) // a waiting row ends; a later row keeps the view
						}
						if r, _ = h.slotRow(ctx, h.db, r.ID); r != nil && r.Active() {
							if b, ok := blocks[v.Block]; ok && v.Status == "open" {
								h.placed(ctx, v, b)
							}
							byTab[v.Tab] = v
							continue
						}
						delete(reserved, v.Tab)
					}
				}
				switch v.Status {
				case "prepared":
					h.closeViewRow(ctx, v.ID, "abandoned") // the request was never sent
					continue
				case "sending":
					if b := h.findViewBlock(ctx, sess, blocks, v.ID); b != "" {
						if err := h.adoptView(ctx, v, sess, blocks[b]); err != nil {
							return false, err
						}
					} else {
						h.closeViewRow(ctx, v.ID, "abandoned")
						continue
					}
				case "exited":
					h.closeView(ctx, v, "renderer exited")
					continue
				case "open":
					b, ok := blocks[v.Block]
					if !ok {
						// Gone while the hub did not watch. Ambiguous (Codex's review): close the view
						// only; the tab stays declared, so a new view opens.
						h.closeViewRow(ctx, v.ID, "missing")
						h.ev.emit("view_missing", map[string]any{"plugin": plugin, "view_id": v.ID, "tab": v.Tab})
						continue
					}
					// A renderer that exited while the hub did not run sent its child_exited to nobody.
					// An attached renderer is alive; check the others. The pane keeps open with no child.
					if v.Attachment == "" {
						if pid, err := h.childIn(ctx, sess, v.Block); err == nil && pid == 0 {
							h.unseenExit(ctx, v)
							continue
						}
					}
					h.placed(ctx, v, b) // the window is derived from Rex, never trusted from the row
				}
				if prev := byTab[v.Tab]; prev != nil {
					h.closeView(ctx, prev, "duplicate") // one view per tab in v1
				}
				byTab[v.Tab] = v
			}

			// 2. Close views whose tab is no longer wanted, or whose route changed.
			want := map[string]tab{}
			for _, t := range tabs {
				if !t.Suppressed {
					want[t.Key] = t
				}
			}
			for key, v := range byTab {
				if reserved[key] != nil {
					continue // no close while the reopen runs
				}
				t, ok := want[key]
				if !ok || t.Route != v.Route {
					h.closeView(ctx, v, "undeclared")
					delete(byTab, key)
					continue
				}
			}

			// 4. Open missing views (bounded per pass) and restore detached ones (a pane the
			// user detached).
			var order []tab
			for _, t := range tabs {
				if _, ok := want[t.Key]; !ok {
					continue
				}
				v := byTab[t.Key]
				if v == nil && movedTab[t.Key] {
					continue // the moved view's pane closes first: no second renderer
				}
				if v == nil && reserved[t.Key] != nil {
					continue // the reopen opens the view
				}
				if v == nil {
					if h.spawnBudget(&more) {
						v, err = h.spawnView(ctx, plugin, sess, t)
						if err != nil {
							return false, err
						}
						byTab[t.Key] = v
					}
				} else if v.Window == "" {
					if err := h.restoreView(ctx, sess, v, t.Title); err != nil {
						return false, err
					}
				}
				if v != nil && v.Window != "" {
					order = append(order, t)
					byKey[plugin+"/"+t.Key] = v
				}
			}

			// 5. Write the declared titles back. The hub never orders tabs: the LazyRex session and
			// the home sessions keep the order that Rex and the user give them.
			if err := h.titleTabs(ctx, sess, order, byTab); err == errRetry {
				more = true
			} else if err != nil {
				return false, err
			}
		}
	}

	// 7. Navigation: show the target tab on every connected app, once it is mounted. The intent
	// is durable. A newer navigation replaces it; this pass then skips the stale target, and the
	// next pass (the navigation kicked it) shows the new one.
	nav, err := h.uiNav(ctx)
	if err != nil {
		return false, err
	}
	if nav != "" {
		v := byKey[nav]
		if v == nil {
			return more, nil // not mounted yet; a later pass navigates
		}
		// The intent stays until Rex confirms the focus; then only this same intent goes. A
		// crash in between shows the target again (focus is idempotent).
		if current, err := h.uiNav(ctx); err != nil || current != nav {
			return true, err // a newer navigation: the next pass shows it
		}
		if err := h.showOnApps(ctx, v.Plugin, v.Session, v); err != nil {
			return false, err
		}
		if _, err := h.db.ExecContext(ctx, `DELETE FROM ui_state WHERE key = 'nav' AND value = ?`, nav); err != nil {
			return false, err
		}
	}
	return more, nil
}

// retryLater kicks the worker again in 5 s, once (repeated calls while one waits do nothing).
func (h *Hub) retryLater() {
	h.ui.mu.Lock()
	defer h.ui.mu.Unlock()
	if h.ui.retryArmed {
		return
	}
	h.ui.retryArmed = true
	time.AfterFunc(5*time.Second, func() {
		h.ui.mu.Lock()
		h.ui.retryArmed = false
		h.ui.mu.Unlock()
		h.kickUI("")
	})
}

// spawnBudget bounds a spawn storm: at most maxSpawnsPerPass new views per pass.
func (h *Hub) spawnBudget(more *bool) bool {
	if h.ui.spawned >= maxSpawnsPerPass {
		*more = true
		return false
	}
	h.ui.spawned++
	return true
}

// placed copies the block's placement from Rex into the view and its row.
func (h *Hub) placed(ctx context.Context, v *view, b rexBlock) {
	w := ""
	if b.Placement != "detached" {
		w = b.WindowID
	}
	if w != v.Window {
		v.Window = w
		h.db.ExecContext(ctx, `UPDATE views SET window_id = ? WHERE view_id = ?`, w, v.ID)
	}
}

// restoreView puts a detached view back in a tab of its own. Rex reattaches a detached block only
// beside a visible anchor (session.move_block), so the hub opens a tab with a placeholder pane,
// moves the view beside it and closes the placeholder. The placeholder's label and arguments
// name the view: closePlaceholders proves ownership with both before it closes one.
func (h *Hub) restoreView(ctx context.Context, sess string, v *view, title string) error {
	nonce := randomID("ph-")
	if _, err := h.db.ExecContext(ctx, `INSERT INTO ui_restores VALUES (?, ?, ?)`, nonce, v.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	req := map[string]any{"session_id": sess, "window_label": title, "focus": false,
		"layout": map[string]any{"block": map[string]any{
			"flavor": "com.superlogical.terminal.shell",
			"label":  "lazyrex-placeholder:" + nonce,
			"options": map[string]any{
				// A loop, so the shell does not exec sleep: its arguments must keep the nonce.
				"command": []string{"/bin/sh", "-c", "while :; do sleep 600; done", "lazyrex-placeholder", nonce},
				"login":   false,
			},
		}}}
	var out struct {
		WindowID string   `json:"window_id"`
		BlockIDs []string `json:"block_ids"`
	}
	if err := h.rex.Call(ctx, "session.new_window", req, &out); err != nil {
		return fmt.Errorf("restore %s: %w", v.ID, err) // an unknown outcome: closePlaceholders finds it
	}
	if len(out.BlockIDs) != 1 {
		return fmt.Errorf("restore %s: %d blocks", v.ID, len(out.BlockIDs))
	}
	ph := out.BlockIDs[0]
	if err := h.rex.Call(ctx, "session.move_block", map[string]any{"session_id": sess, "block_id": v.Block, "anchor_block_id": ph}, nil); err != nil {
		return fmt.Errorf("restore %s: move: %w", v.ID, err) // the next pass closes the placeholder
	}
	if err := h.closeBlock(ctx, sess, ph); err != nil {
		return fmt.Errorf("restore %s: close placeholder: %w", v.ID, err) // the record stays; cleanup retries
	}
	h.db.ExecContext(ctx, `DELETE FROM ui_restores WHERE nonce = ?`, nonce)
	v.Window = out.WindowID
	h.db.ExecContext(ctx, `UPDATE views SET window_id = ? WHERE view_id = ?`, v.Window, v.ID)
	h.ev.emit("view_restored", map[string]any{"plugin": v.Plugin, "view_id": v.ID, "tab": v.Tab, "window_id": v.Window})
	return nil
}

// closeBlock closes a block the hub owns. A block that is already gone counts as closed.
func (h *Hub) closeBlock(ctx context.Context, sess, block string) error {
	h.mu.Lock()
	h.selfClosed[block] = true
	h.mu.Unlock()
	err := h.rex.Call(ctx, "block.close", map[string]any{"session_id": sess, "block_id": block}, nil)
	var re *rex.RexError
	if err != nil && errors.As(err, &re) && re.Code == "not_found" {
		return nil
	}
	return err
}

// closePlaceholders closes placeholder panes that a crash or a failed move or close left
// behind, before the pass restores anything. A pane qualifies only when its label and its
// process arguments carry the nonce of a recorded restore (proven ownership). A record goes only
// when its pane is confirmed closed. A record with no pane stays: the earlier new_window may
// still complete, and nothing proves it will not.
func (h *Hub) closePlaceholders(ctx context.Context, sess string, blocks map[string]rexBlock) error {
	rows, err := h.db.QueryContext(ctx, `SELECT nonce FROM ui_restores`)
	if err != nil {
		return err
	}
	var nonces []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		nonces = append(nonces, n)
	}
	rows.Close()
	for _, nonce := range nonces {
		for id, b := range blocks {
			if b.Label != "lazyrex-placeholder:"+nonce {
				continue
			}
			pid, err := h.childIn(ctx, sess, id)
			if err != nil || pid == 0 || !argvHas(pid, nonce) || !argvHas(pid, "lazyrex-placeholder") {
				h.ev.emit("placeholder_unproven", map[string]any{"block_id": id, "nonce": nonce})
				continue // not proven ours: leave it
			}
			if err := h.closeBlock(ctx, sess, id); err != nil {
				return fmt.Errorf("close placeholder %s: %w", id, err)
			}
			delete(blocks, id)
			h.db.ExecContext(ctx, `DELETE FROM ui_restores WHERE nonce = ?`, nonce)
			h.ev.emit("placeholder_closed", map[string]any{"block_id": id, "nonce": nonce})
		}
	}
	return nil
}

// findViewBlock finds the pane of a spawn whose outcome is unknown. The label is a hint;
// the child's arguments must name the view.
func (h *Hub) findViewBlock(ctx context.Context, sess string, blocks map[string]rexBlock, id string) string {
	for b, info := range blocks {
		if info.Label != "lazyrex-view:"+id {
			continue
		}
		if c, err := h.childIn(ctx, sess, b); err == nil && c > 0 && argvHas(c, id) {
			return b
		}
	}
	return ""
}

func (h *Hub) adoptView(ctx context.Context, v *view, sess string, b rexBlock) error {
	pid, err := h.childIn(ctx, sess, b.BlockID)
	if err != nil {
		return err
	}
	v.Status, v.Session, v.Window, v.Block, v.ChildPID, v.ChildStart = "open", sess, b.WindowID, b.BlockID, pid, startTime(pid)
	_, err = h.db.ExecContext(ctx, `UPDATE views SET status = 'open', session_id = ?, window_id = ?, block_id = ?, child_pid = ?, child_start = ?
		WHERE view_id = ?`, sess, v.Window, v.Block, v.ChildPID, v.ChildStart, v.ID)
	return err
}

func (h *Hub) spawnView(ctx context.Context, plugin, sess string, t tab) (*view, error) {
	if h.cfg.View == "" {
		return nil, errors.New("no renderer: set \"view\" in the hub config")
	}
	v := &view{ID: randomID("view-"), Plugin: plugin, Tab: t.Key, Route: t.Route, Status: "prepared"}
	// The pass read its tabs before: a crash limit or a user close since then suppressed the tab, and
	// the insert reads that in the same statement. No view then (nil).
	res, err := h.db.ExecContext(ctx, `INSERT INTO views (view_id, plugin, tab, route, status, session_id, attach_state)
		SELECT ?, ?, ?, ?, 'prepared', ?, 'never' WHERE NOT EXISTS (SELECT 1 FROM ui_tabs WHERE plugin = ? AND tab = ? AND suppressed = 1)`,
		v.ID, plugin, t.Key, t.Route, sess, plugin, t.Key)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return nil, err
	}
	h.markOwn(v.ID)
	if _, err := h.db.ExecContext(ctx, `UPDATE views SET status = 'sending' WHERE view_id = ?`, v.ID); err != nil {
		return nil, err
	}
	req := map[string]any{
		"session_id":   sess,
		"window_label": t.Title,
		"focus":        false, // only an explicit navigation changes what the user sees
		"layout": map[string]any{"block": map[string]any{
			"flavor": "com.superlogical.terminal.shell",
			"label":  "lazyrex-view:" + v.ID,
			"options": map[string]any{
				"command": h.viewCommand(v.ID, plugin, t.Route),
				"login":   false,
				// Keep-open: a renderer crash is child_exited, not a pane close (rex-protocol.md).
				"exit": map[string]any{"on_completion": false},
			},
		}},
	}
	var out struct {
		WindowID string   `json:"window_id"`
		BlockIDs []string `json:"block_ids"`
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := viewNewWindow(h, cctx, req, &out); err != nil {
		var re *rex.RexError
		if errors.As(err, &re) && re.Code != "disconnected" {
			h.closeViewRow(ctx, v.ID, "abandoned") // Rex answered: nothing was created
		}
		return nil, fmt.Errorf("open tab %s: %w", t.Key, err) // otherwise 'sending' stays unknown
	}
	if len(out.BlockIDs) != 1 {
		return nil, fmt.Errorf("open tab %s: %d blocks", t.Key, len(out.BlockIDs))
	}
	if err := h.adoptView(ctx, v, sess, rexBlock{BlockID: out.BlockIDs[0], WindowID: out.WindowID}); err != nil {
		return nil, err
	}
	h.ev.emit("view_opened", map[string]any{"plugin": plugin, "view_id": v.ID, "tab": t.Key, "route": t.Route, "window_id": v.Window})
	return v, nil
}

// viewNewWindow is session.new_window for a view's tab; lab tests replace it to lose the answer.
var viewNewWindow = func(h *Hub, ctx context.Context, req map[string]any, out any) error {
	return h.rex.Call(ctx, "session.new_window", req, out)
}

// closeView closes the view's tab. The hub marks its own closes, so the close event is not
// taken as the user's.
func (h *Hub) closeView(ctx context.Context, v *view, why string) {
	h.mu.Lock()
	h.selfClosed[v.Block] = true
	h.mu.Unlock()
	if err := h.rex.Call(ctx, "block.close", map[string]any{"session_id": v.Session, "block_id": v.Block}, nil); err != nil {
		log.Printf("ui: close %s: %v", v.ID, err)
	}
	h.closeViewRow(ctx, v.ID, "hub")
	h.ev.emit("view_closed", map[string]any{"plugin": v.Plugin, "view_id": v.ID, "tab": v.Tab, "by": "hub", "why": why})
}

// closeMoved closes a view that is in a session that is not its tab's target: its exact pane
// first, then its row. It reports whether both are done. A failed pane close keeps the row, so the
// tab opens no second view and a later pass tries again (codex-25's review of the view.retire
// removal). A pane that is already gone counts as closed (closeBlock).
func (h *Hub) closeMoved(ctx context.Context, v *view) bool {
	if v.Block != "" {
		if err := h.closeBlock(ctx, v.Session, v.Block); err != nil {
			log.Printf("ui: close moved %s: %v", v.ID, err)
			return false
		}
	}
	h.closeViewRow(ctx, v.ID, "hub")
	h.ev.emit("view_closed", map[string]any{"plugin": v.Plugin, "view_id": v.ID, "tab": v.Tab, "by": "hub", "why": "moved"})
	return true
}

// titleTabs writes each open tab's declared title to its window label.
func (h *Hub) titleTabs(ctx context.Context, sess string, tabs []tab, byTab map[string]*view) error {
	title := map[string]string{} // window -> declared title
	for _, t := range tabs {
		if v := byTab[t.Key]; v != nil {
			title[v.Window] = t.Title
		}
	}
	var sv struct {
		Windows []struct {
			WindowID string `json:"window_id"`
			Label    string `json:"label"`
		} `json:"windows"`
	}
	if err := h.rex.Call(ctx, "session.view", map[string]any{"session_id": sess}, &sv); err != nil {
		return err
	}
	// A plugin may change a tab's title (for example an unread count). Rename the open tab to match.
	for _, w := range sv.Windows {
		if t, ok := title[w.WindowID]; ok && t != "" && w.Label != t && !h.renamePending(w.WindowID) {
			h.expectLabel(w.WindowID, t)
			args := map[string]any{"session_id": sess, "window_id": w.WindowID, "label": t}
			if err := h.rex.Call(ctx, "session.set_window_label", args, nil); err != nil {
				h.unexpectLabel(w.WindowID, t) // no echo comes (if Rex applied it anyway, one rename reaches the plugin with the declared title)
				var re *rex.RexError
				if errors.As(err, &re) && re.Code == "not_found" {
					return errRetry
				}
				return err
			}
			h.ev.emit("tab_renamed", map[string]any{"session_id": sess, "window_id": w.WindowID, "title": t})
		}
	}
	return nil
}

// showOnApps sends one navigation to every connected app that can select a session.
// Each app has its own result; one failing app does not block the others.
// It returns the session focus error: the app selection is best effort per app.
func (h *Hub) showOnApps(ctx context.Context, plugin, sess string, v *view) error {
	if err := h.rex.Call(ctx, "session.focus_block", map[string]any{"session_id": sess, "block_id": v.Block}, nil); err != nil {
		return fmt.Errorf("focus %s: %w", v.ID, err)
	}
	// An app now shows this view. The renderer may get no focus-in for a pane that already has
	// focus (DECSET 1004 does not report the initial state), so the navigation counts as seen.
	if h.selectOnApps(ctx, sess, v.Window, map[string]any{"plugin": plugin, "tab": v.Tab}) {
		h.markSeen(plugin, v.Route, "focus") // a navigation shows the view: as a focus
	}
	return nil
}

// selectOnApps asks every connected app that can select a session to show sess (and window, when
// it is set). It reports whether at least one app did. Each app's result is an event.
func (h *Hub) selectOnApps(ctx context.Context, sess, window string, about map[string]any) bool {
	var cl struct {
		Clients []struct {
			ClientID string `json:"client_id"`
		} `json:"clients"`
	}
	if err := h.rex.Call(ctx, "client.list", map[string]any{}, &cl); err != nil {
		log.Printf("ui: client.list: %v", err)
		return false
	}
	shown := false
	for _, c := range cl.Clients {
		var ins struct {
			Client struct {
				Info struct {
					Kind string `json:"kind"`
				} `json:"info"`
			} `json:"client"`
			Actions []struct {
				Name string `json:"name"`
			} `json:"actions"`
		}
		if h.rex.Call(ctx, "client.inspect", map[string]any{"client_id": c.ClientID}, &ins) != nil || ins.Client.Info.Kind != "app" {
			continue
		}
		can := false
		for _, a := range ins.Actions {
			can = can || a.Name == "session.select"
		}
		result := "selected"
		if !can {
			result = "no session.select"
		} else {
			args := map[string]any{"session_id": sess}
			if window != "" {
				args["window_id"] = window
			}
			if err := appPerform(h, ctx, c.ClientID, "session.select", args); err != nil {
				result = err.Error()
			}
		}
		ev := map[string]any{"app": c.ClientID, "session_id": sess, "result": result}
		for k, v := range about {
			ev[k] = v
		}
		h.ev.emit("app_navigated", ev)
		if result == "selected" {
			shown = true
		}
	}
	return shown
}

// uiBlockClosed handles a closed view pane. It reports whether the block was a view.
func (h *Hub) uiBlockClosed(ctx context.Context, blockID string, self bool) bool {
	var id, plugin, tabKey, sess string
	err := h.db.QueryRowContext(ctx, `SELECT view_id, plugin, tab, session_id FROM views WHERE block_id = ? AND status = 'open'`, blockID).Scan(&id, &plugin, &tabKey, &sess)
	if err != nil {
		return !self && h.tabReplPaneClosed(ctx, blockID) // a reopen's new pane before its adoption
	}
	if tabKey == formTab {
		if !self {
			fc, _ := h.formOf(ctx, id)
			if won, err := h.closeViewOnce(ctx, id, "user", false); !won {
				if err != nil {
					log.Printf("ui: close form %s: %v", id, err)
				}
				return true // another close of this form reported it
			}
			h.slotOldClosed(ctx, id)
			h.ev.emit("form_closed", map[string]any{"plugin": plugin, "view_id": id, "by": "user"})
			go h.formEnded(context.Background(), id, fc, false, "")
		}
		return true
	}
	if !self {
		// Not the hub's close: the view closes, the data stays, and the tab stays closed until
		// the next navigation to it (architecture.md, "Closes").
		h.closeViewRow(ctx, id, "user")
		h.db.ExecContext(ctx, `UPDATE ui_tabs SET suppressed = 1, closed_revision = revision WHERE plugin = ? AND tab = ?`, plugin, tabKey)
		h.ev.emit("view_closed", map[string]any{"plugin": plugin, "view_id": id, "tab": tabKey, "by": "user"})
		h.tabReplUserClosed(ctx, id) // the user closed the tab: a reopen of it ends
	}
	h.kickUI(plugin)
	return true
}

// viewClose is the user's close from inside a view (Ctrl+Q in the renderer, hub.view.close): the
// same as a close of the pane. The tab stays closed until a new revision or a
// navigation; the hub then closes the pane. A renderer that quits without this call counts a crash.
// The renderer checks its own draft and operation gate before the call (Codex's review).
func (h *Hub) viewClose(ctx context.Context, p perform, args map[string]any) (any, error) {
	// A repeat by the same attachment (a lost answer) gets the same answer: the closed row keeps
	// its client and attachment for this check only. Every other reader asks for status 'open'.
	var n int
	h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM views WHERE view_id = ? AND status = 'closed' AND closed_by = 'user-view'
		AND attachment = ? AND attachment != '' AND client_id = ?`, args["view_id"], args["attachment"], p.Requester.ClientID).Scan(&n)
	if n == 1 {
		return map[string]any{"status": "closed"}, nil
	}
	v, err := h.boundCaller(ctx, p, args)
	if err != nil {
		return nil, err
	}
	var tabKey, sess, block string
	if err := h.db.QueryRowContext(ctx, `SELECT tab, session_id, block_id FROM views WHERE view_id = ?`, v.ID).Scan(&tabKey, &sess, &block); err != nil {
		return nil, err
	}
	if tabKey == formTab {
		// A panel closes here (Esc or Ctrl+Q); a form closes with its cancel action.
		if fc, _ := h.formOf(ctx, v.ID); !fc.Panel {
			return nil, bad("a form closes with its cancel action")
		}
		won, err := h.closeViewOnce(ctx, v.ID, "user-view", true)
		if err != nil {
			return nil, err
		}
		if !won {
			return map[string]any{"status": "closed"}, nil // its exit or its pane's close was first
		}
		h.slotOldClosed(ctx, v.ID)
		h.ev.emit("form_closed", map[string]any{"plugin": v.Plugin, "view_id": v.ID, "by": "user", "panel": true})
		go func() {
			time.Sleep(300 * time.Millisecond) // the renderer answers, then exits
			h.mu.Lock()
			h.selfClosed[block] = true
			h.mu.Unlock()
			if err := h.closeBlock(context.Background(), sess, block); err != nil {
				log.Printf("ui: close panel %s: %v", v.ID, err)
			}
		}()
		return map[string]any{"status": "closed"}, nil
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE views SET status = 'closed', closed_by = 'user-view' WHERE view_id = ?`, v.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ui_tabs SET suppressed = 1, closed_revision = revision WHERE plugin = ? AND tab = ?`, v.Plugin, tabKey); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	h.forgetOwn(v.ID)
	h.forgetView(v.ID)
	h.ev.emit("view_closed", map[string]any{"plugin": v.Plugin, "view_id": v.ID, "tab": tabKey, "by": "user", "from": "view"})
	h.tabReplUserClosed(ctx, v.ID)
	// The renderer exits after the answer; its pane is closed here. If this fails, the pane stays
	// with no child: the user closes it, and its close changes nothing (the row is closed).
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := h.closeBlock(context.Background(), sess, block); err != nil {
			log.Printf("ui: close %s: %v", v.ID, err)
		}
	}()
	h.kickUI(v.Plugin)
	return map[string]any{"status": "closed"}, nil
}

// uiChildExited handles a renderer that exited in its kept-open pane. It only records state:
// the plugin's worker closes the tab and opens a new one, up to maxCrashes times per tab.
// A navigation resets the count.
func (h *Hub) uiChildExited(ctx context.Context, blockID string) bool {
	v, err := h.viewByBlock(ctx, blockID)
	if err != nil || v == nil {
		return false
	}
	if v.Tab == formTab {
		// A form ends with its renderer; its pane closes itself (exit on_completion). No crash
		// count: a form is not a tab. A cancelled form gives the slot back to its panel.
		fc, _ := h.formOf(ctx, v.ID)
		if won, err := h.closeViewOnce(ctx, v.ID, "exited", false); !won {
			if err != nil {
				log.Printf("ui: close form %s: %v", v.ID, err)
			}
			return true // another close of this form reported it
		}
		h.slotOldClosed(ctx, v.ID)
		h.ev.emit("form_closed", map[string]any{"plugin": v.Plugin, "view_id": v.ID, "by": "exit"})
		go h.formEnded(context.Background(), v.ID, fc, false, "")
		return true
	}
	h.rendererExited(ctx, v)
	h.kickUI(v.Plugin)
	return true
}

// rendererExited records the renderer exit of an open view: from Rex's child_exited, or from a pass
// that finds the renderer of an own view gone (reconcile). Only the call whose update moves the
// row from open to exited ends a waiting reopen and counts the crash. So the event and the pass
// count one exit once, in either order and also when both read the row as open. The row's change
// and the tab's count and suppression commit together: a pass that finds the row exited or closed
// cannot open the tab again in the window before the suppression (spawnView reads it).
func (h *Hub) rendererExited(ctx context.Context, v *view) {
	crashes, moved, err := h.countExit(ctx, v)
	if err != nil {
		log.Printf("ui: renderer exit of %s: %v", v.ID, err)
		return
	}
	if !moved {
		return // another call moved the row
	}
	h.tabReplExited(ctx, v.ID) // before the pane request a reopen ends; later it goes on
	h.ev.emit("view_exited", map[string]any{"plugin": v.Plugin, "view_id": v.ID, "tab": v.Tab, "crashes": crashes})
	if crashes >= maxCrashes {
		h.ev.emit("view_failed", map[string]any{"plugin": v.Plugin, "tab": v.Tab})
	}
}

// countExit moves an open view row to exited and counts the crash of its tab, in one transaction.
// At maxCrashes it suppresses the tab. It reports moved = false when the row was not open.
func (h *Hub) countExit(ctx context.Context, v *view) (crashes int, moved bool, err error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE views SET status = 'exited', client_id = '', attachment = '' WHERE view_id = ? AND status = 'open'`, v.ID)
	if err != nil {
		return 0, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return 0, false, err
	}
	if err := tx.QueryRowContext(ctx, `UPDATE ui_tabs SET crashes = crashes + 1, suppressed = CASE WHEN crashes + 1 >= ? THEN 1 ELSE suppressed END
		WHERE plugin = ? AND tab = ? RETURNING crashes`, maxCrashes, v.Plugin, v.Tab).Scan(&crashes); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	return crashes, true, tx.Commit()
}

// unseenExit closes an open, unattached view whose pane has no child (a pass found it). For a view
// that this hub process opened, the exit event can still come, or the pass can be first: either one
// counts the crash, once (rendererExited). A dead renderer of an earlier hub process counts none,
// so hub restarts never suppress a tab.
func (h *Hub) unseenExit(ctx context.Context, v *view) {
	if h.ownView(v.ID) {
		h.rendererExited(ctx, v)
	}
	h.closeView(ctx, v, "renderer exited unseen")
}

func (h *Hub) viewByBlock(ctx context.Context, blockID string) (*view, error) {
	v := &view{}
	err := h.db.QueryRowContext(ctx, `SELECT view_id, plugin, tab, route, status, session_id, window_id, block_id, child_pid, child_start,
		client_id, attachment FROM views WHERE block_id = ? AND status = 'open'`, blockID).Scan(&v.ID, &v.Plugin, &v.Tab, &v.Route,
		&v.Status, &v.Session, &v.Window, &v.Block, &v.ChildPID, &v.ChildStart, &v.ClientID, &v.Attachment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

// uiSessionChanged reacts to layout changes in the LazyRex session (snap back), and in a session
// with a side-slot replacement.
func (h *Hub) uiSessionChanged(sess string) {
	h.ui.mu.Lock()
	key := h.ui.sess[sess]
	h.ui.mu.Unlock()
	if key == uiSessionKey {
		h.kickUI("")
	}
	// An unresolved side-slot replacement: its pane can appear in this session.
	if h.slotSessions(context.Background(), sess) {
		go h.slotSweep(context.Background())
	}
}

func (h *Hub) uiSessionDestroyed(ctx context.Context, sess string) {
	h.ui.mu.Lock()
	_, ok := h.ui.sess[sess]
	h.ui.mu.Unlock()
	if ok {
		// The LazyRex session comes back at once. Another recorded session is gone.
		h.forgetSession(ctx, sess, "destroyed")
		h.kickUI("")
	}
}

// --- Actions ---

// uiTabs replaces the calling plugin's declared tab set. An empty list is valid for any plugin:
// a plugin with only a landing panel has no tabs (panels.go). The home plugin declares no tabs:
// it has only its panel and forms.
func (h *Hub) uiTabs(ctx context.Context, plugin string, args map[string]any) (any, error) {
	list, ok := asList(args["tabs"])
	if !ok {
		return nil, bad("tabs must be a list")
	}
	if len(list) > 0 && plugin == h.homePlugin() {
		return nil, bad("the home plugin declares no tabs")
	}
	seen := map[string]bool{}
	var tabs []tab
	for i, item := range list {
		m, _ := item.(map[string]any)
		key, _ := m["key"].(string)
		title, _ := m["title"].(string)
		route, _ := m["route"].(string)
		if key == "" || title == "" || !strings.HasPrefix(route, "/") || seen[key] {
			return nil, bad("tab %d needs a unique key, a title and a route that starts with /", i)
		}
		home, okHome := m["session_id"].(string)
		rev, okRev := m["revision"].(string)
		if (m["session_id"] != nil && (!okHome || (home != "" && !strings.HasPrefix(home, "session:")))) || (m["revision"] != nil && !okRev) {
			return nil, bad("tab %d: session_id must be a Rex session ID and revision must be text", i)
		}
		status, err := parseTabStatus(m["status"])
		if err != nil {
			return nil, bad("tab %d: %v", i, err)
		}
		seen[key] = true
		tabs = append(tabs, tab{Key: key, Title: title, Route: route, Pos: i, Session: home, Revision: rev, Status: status})
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Keep the suppression and crash count of tabs that stay declared. A plugin declares again
	// when only a title changes (for example an unread count), so this must not reset them.
	type kept struct {
		suppressed bool
		crashes    int
		closedRev  string // the revision at the manual close
		status     string
	}
	old := map[string]kept{}
	rows, err := tx.QueryContext(ctx, `SELECT tab, suppressed, crashes, closed_revision, status FROM ui_tabs WHERE plugin = ?`, plugin)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var o kept
		if err = rows.Scan(&k, &o.suppressed, &o.crashes, &o.closedRev, &o.status); err != nil {
			break
		}
		old[k] = o
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err // never replace the rows with suppression state half read
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ui_tabs WHERE plugin = ?`, plugin); err != nil {
		return nil, err
	}
	statusChanged := false
	for _, t := range tabs {
		o := old[t.Key]
		statusChanged = statusChanged || o.status != t.Status
		if o.suppressed && t.Revision != "" && t.Revision != o.closedRev {
			// Something new since the user closed the tab (a new message): it opens again.
			o.suppressed = false
			h.ev.emit("tab_reopened", map[string]any{"plugin": plugin, "tab": t.Key, "revision": t.Revision})
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ui_tabs (plugin, tab, title, route, pos, suppressed, crashes, session_id, revision, closed_revision, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, plugin, t.Key, t.Title, t.Route, t.Pos, o.suppressed, o.crashes, t.Session, t.Revision, o.closedRev, t.Status); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	h.kickUI(plugin)
	if statusChanged {
		h.kickViews(plugin, causeStatus) // the snapshots carry the declared status
	}
	return map[string]any{"tabs": len(tabs)}, nil
}

// uiNavigate records a navigation and returns. The worker opens the tab if needed and shows it on
// every app. A newer navigation replaces an older one that is not done.
// navigationTab lets an explicit navigation declare a lazy tab once. Restore and reconciliation
// use only existing declarations. The plugin interprets the route derived from the tab key.
func (h *Hub) navigationTab(ctx context.Context, plugin, key string, open func(context.Context, string, string, any, any) error) error {
	declared := func() (bool, error) {
		var n int
		err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ui_tabs WHERE plugin = ? AND tab = ?`, plugin, key).Scan(&n)
		return n > 0, err
	}
	found, err := declared()
	if err != nil || found {
		return err
	}
	if plugin != "" && key != "" {
		// A plugin without view.open keeps the normal missing-tab error. A lost answer can still
		// leave a declaration, so read the declaration again before deciding the result.
		_ = open(ctx, plugin, "view.open", map[string]any{"route": "/" + strings.TrimPrefix(key, "/"), "user": viewUser}, nil)
		found, err = declared()
		if err != nil || found {
			return err
		}
	}
	return bad("plugin %q has no tab %q", plugin, key)
}

// uiNavigate handles hub.ui.navigate {plugin, tab}. A declared tab is committed at once:
// {status: "accepted"}. An undeclared tab needs the plugin's view.open, and a hub handler must
// never wait for a plugin: the call starts view.open in the background and answers
// {status: "pending", navigation_id}. A caller that wants the outcome asks again with
// {navigation_id} (plugin and tab are then ignored): pending until the open finishes, then the
// result: accepted, superseded (a newer navigation came first), or the error (for example "has no
// tab"). A call without navigation_id is always a new navigation. A caller that does not ask again
// still gets the navigation.
func (h *Hub) uiNavigate(ctx context.Context, args map[string]any) (any, error) {
	if id, ok := args["navigation_id"].(string); ok {
		h.ui.navMu.Lock()
		defer h.ui.navMu.Unlock()
		f := h.ui.navOpens[id]
		if f == nil {
			return nil, refuse("not_found", "navigation %q is unknown or expired", id)
		}
		if !f.done {
			return map[string]any{"status": "pending", "navigation_id": id}, nil
		}
		return f.out, f.err
	}
	plugin, _ := args["plugin"].(string)
	key, _ := args["tab"].(string)
	h.ui.navMu.Lock()
	h.ui.navSeq++
	seq := h.ui.navSeq
	for id, f := range h.ui.navOpens {
		if f.done && time.Since(f.started) > time.Minute {
			delete(h.ui.navOpens, id) // its caller stopped asking
		}
	}
	h.ui.navMu.Unlock()
	declared, err := h.tabDeclared(ctx, plugin, key)
	if err != nil {
		return nil, err
	}
	if declared {
		return h.navigateCommit(ctx, plugin, key, seq)
	}
	// The generation keeps an ID from an earlier hub process from naming a new navigation.
	id := "nav-" + revString(h.generation) + "-" + strconv.FormatInt(seq, 10)
	f := &navOpen{seq: seq, started: time.Now()}
	h.ui.navMu.Lock()
	if h.ui.navOpens == nil {
		h.ui.navOpens = map[string]*navOpen{}
	}
	h.ui.navOpens[id] = f
	h.ui.navMu.Unlock()
	go func() {
		out, err := h.navigateWith(context.Background(), plugin, key, seq)
		h.ui.navMu.Lock()
		f.done, f.out, f.err = true, out, err
		h.ui.navMu.Unlock()
	}()
	return map[string]any{"status": "pending", "navigation_id": id, "plugin": plugin, "tab": key}, nil
}

// navigateNow navigates and waits for the result. Only a caller outside the hub's request
// handlers may use it: the view worker for a navigate effect (applyEffects). A navigation that a
// newer one superseded is done, not failed.
func (h *Hub) navigateNow(ctx context.Context, plugin, key string) error {
	h.ui.navMu.Lock()
	h.ui.navSeq++
	seq := h.ui.navSeq
	h.ui.navMu.Unlock()
	_, err := h.navigateWith(ctx, plugin, key, seq)
	return err
}

// navigateWith lets the plugin declare a lazy tab (navigationTab, which may call view.open), then
// commits the navigation.
func (h *Hub) navigateWith(ctx context.Context, plugin, key string, seq int64) (map[string]any, error) {
	if err := h.navigationTab(ctx, plugin, key, h.callPlugin); err != nil {
		return nil, err
	}
	return h.navigateCommit(ctx, plugin, key, seq)
}

func (h *Hub) tabDeclared(ctx context.Context, plugin, key string) (bool, error) {
	var n int
	err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ui_tabs WHERE plugin = ? AND tab = ?`, plugin, key).Scan(&n)
	return n > 0, err
}

// navigateCommit records the navigation target. A request that a newer navigation followed does
// not commit: an old completion must not replace a newer navigation. navMu covers the check and
// the database commit, so two commits cannot pass each other. The event and the kicks run after it.
func (h *Hub) navigateCommit(ctx context.Context, plugin, key string, seq int64) (map[string]any, error) {
	committed, err := h.navigateCommitLocked(ctx, plugin, key, seq)
	if err != nil || !committed {
		if err == nil {
			return map[string]any{"status": "superseded", "plugin": plugin, "tab": key}, nil
		}
		return nil, err
	}
	h.ev.emit("navigation_committed", map[string]any{"plugin": plugin, "tab": key, "seq": seq})
	h.kickUI(plugin)
	return map[string]any{"status": "accepted"}, nil
}

func (h *Hub) navigateCommitLocked(ctx context.Context, plugin, key string, seq int64) (bool, error) {
	h.ui.navMu.Lock()
	defer h.ui.navMu.Unlock()
	if seq != h.ui.navSeq {
		return false, nil
	}
	// The target commits first: a crash then resumes the focus.
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE ui_tabs SET suppressed = 0, crashes = 0 WHERE plugin = ? AND tab = ?`, plugin, key); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ui_state VALUES ('nav', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, plugin+"/"+key); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// uiReload asks every attached renderer of the plugin to run its binary again (view.reload).
// The renderer replaces itself in the same process, so the pane, tab and PID stay. Tabs that
// crashed too often open again. A renderer that does not answer is left alone: it reattaches
// or exits, and the reconciler handles either. The hub waits timeout_ms + performMargin for each
// request (perform.go); Rex can still deliver one after that.
func (h *Hub) uiReload(ctx context.Context, args map[string]any) (any, error) {
	plugin, _ := args["plugin"].(string)
	if _, err := h.db.ExecContext(ctx, `UPDATE ui_tabs SET suppressed = 0, crashes = 0 WHERE plugin = ? AND crashes >= ?`, plugin, maxCrashes); err != nil {
		return nil, err
	}
	views, err := h.loadViews(ctx, plugin)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, v := range views {
		if v.Status != "open" || v.Attachment == "" {
			continue
		}
		n++
		go func(v *view) {
			ctx, cancel := performCtx(context.Background(), 5000)
			defer cancel()
			err := h.rex.Call(ctx, "client.perform", map[string]any{"client_id": v.ClientID, "action": "view.reload",
				"args": map[string]any{"view_id": v.ID, "attachment": v.Attachment}, "timeout_ms": 5000}, nil)
			h.ev.emit("view_reload", map[string]any{"plugin": plugin, "view_id": v.ID, "error": errString(err)})
		}(v)
	}
	h.kickUI(plugin)
	return map[string]any{"status": "accepted", "views": n}, nil
}

// viewAttach binds a renderer to its view. The caller must run in the view's pane.
func (h *Hub) viewAttach(ctx context.Context, p perform, args map[string]any) (any, error) {
	id, _ := args["view_id"].(string)
	if sv, _ := args["schema_version"].(float64); sv != 1 {
		return nil, bad("unsupported_schema: this hub speaks schema version 1")
	}
	if role, _ := args["role"].(string); role != "" && role != "composer" {
		return nil, bad("unknown role %q", role)
	}
	var v view
	err := h.db.QueryRowContext(ctx, `SELECT plugin, route, status, child_pid, child_start FROM views WHERE view_id = ?`, id).
		Scan(&v.Plugin, &v.Route, &v.Status, &v.ChildPID, &v.ChildStart)
	if err != nil || v.Status == "closed" || v.Status == "exited" {
		return nil, bad("unknown or closed view %q", id)
	}
	if v.Status != "open" {
		// The renderer can start before the hub has recorded its pane. It retries.
		return nil, bad("not_ready: view %q is still opening", id)
	}
	if !alive(v.ChildPID, v.ChildStart) || !descendsFrom(p.Requester.Principal.PID, v.ChildPID) {
		return nil, bad("caller is not in the pane of view %q", id)
	}
	attachment := randomID("att-")
	// One conditional write: a concurrent close wins, and a replacement's claim of a never-attached
	// view (panels.go askRelease) is ordered against it by SQLite.
	res, err := h.db.ExecContext(ctx, `UPDATE views SET client_id = ?, attachment = ?, attach_state = 'attached' WHERE view_id = ? AND status = 'open'`, p.Requester.ClientID, attachment, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, bad("unknown or closed view %q", id)
	}
	h.ev.emit("view_attached", map[string]any{"view_id": id, "client_id": p.Requester.ClientID})
	h.markView(v.Plugin, id, causeAttach) // render the first snapshot for this attachment
	out := map[string]any{"view_id": id, "plugin": v.Plugin, "route": v.Route, "user": viewUser,
		"hub_generation": revString(h.generation), "attachment": attachment}
	// A renderer that waits on a side-slot replacement (frozen or prompting) names it; the
	// durable row's state is the answer (panels.go).
	// A restarted renderer has lost its ID: the hub also reports any active replacement of the view.
	repl, _ := args["replacement_id"].(string)
	if r := h.slotAttachState(ctx, id, repl); r != nil {
		out["replacement"] = r
	}
	return out, nil
}

// uiDescribe reports, for the lab tests, tabs, views and sessions as the hub sees them.
func (h *Hub) uiDescribe(ctx context.Context, args map[string]any) (any, error) {
	plugin, _ := args["plugin"].(string)
	tabs, err := h.loadTabs(ctx, plugin)
	if err != nil {
		return nil, err
	}
	views, err := h.loadViews(ctx, plugin)
	if err != nil {
		return nil, err
	}
	var sess string
	h.db.QueryRowContext(ctx, `SELECT session_id FROM ui_sessions WHERE plugin = ?`, uiSessionKey).Scan(&sess)
	tl := []any{}
	for _, t := range tabs {
		tl = append(tl, map[string]any{"tab": t.Key, "route": t.Route, "suppressed": t.Suppressed, "session_id": t.Session})
	}
	vl := []any{}
	for _, v := range views {
		row := map[string]any{"view_id": v.ID, "tab": v.Tab, "status": v.Status, "session_id": v.Session, "window_id": v.Window, "block_id": v.Block, "attached": v.Attachment != ""}
		vl = append(vl, row)
	}
	// default_session_id: where tabs without a home session live (the LazyRex session).
	// home_redirects: each replaced home session -> the session its tabs use now (hometabs.go).
	redirects, err := h.homeRedirects(ctx)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(map[string]any{"session_id": sess, "default_session_id": sess, "tabs": tl, "views": vl, "home_redirects": redirects})
	var out any
	json.Unmarshal(b, &out)
	return out, nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// viewCommand is the renderer's argv. -server is the hub's own endpoint (hub.json, as loaded): a
// Linux pane has no REX_SERVER, and the renderer never guesses one. On an approved real-server run
// the renderer must accept the real socket, so it gets -real-server too. The renderer argv is an
// interface between two binaries of one release.
func (h *Hub) viewCommand(id, plugin, route string) []string {
	cmd := []string{h.cfg.View, "-view-id", id, "-server", h.cfg.Server}
	if h.cfg.Real {
		cmd = append(cmd, "-real-server")
	}
	return append(cmd, plugin, route)
}

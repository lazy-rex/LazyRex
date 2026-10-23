package main

// Palette commands and forms (the maintainer, 2026-10-17: plugins declare command-palette entries that
// open forms).
//
// A plugin declares commands at hub.register: commands = [{id, title, description?, keywords?,
// route}]. The hub writes one rex.action per command to a Lua file beside the Rex config
// (config "palette_lua"); init.lua loads it with pcall(require, "lazyrex_palette"). The hub runs
// `rex config reload` only when the file content changes. Only config actions appear in the Rex
// palette; client actions do not (rex-protocol.md, "Command palette entries and forms").
//
// A palette run calls client.perform hub.palette.run {command, ctx} on the connected lazyrex-hub
// client. The hub takes the plugin and route from its own command table, never from ctx. It
// checks ctx.session_id and ctx.block_id against Rex, opens the route as a form (a renderer in a
// focused split in that window), and returns {select} for the Lua action to queue
// session.select on the app that ran it.
//
// A form is a view row with tab formTab and a stored context {session_id, window_id,
// source_view_id, source_route}. The plugin gets that context as args.context on every render and
// action. Nothing is recovered from focus later. Forms are not tabs: the tab reconcile, tab rename
// and crash counts skip them. A view action result can open a form ({form: route}), close the form
// that ran it ({close_form: true}), or navigate, which also closes the form that ran it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

const formTab = "@form" // the tab key of every form view

const formSchema = `
-- The palette commands each plugin declared at its last registration (JSON list). Kept across
-- restarts, so the generated palette file does not change while plugins start again.
CREATE TABLE IF NOT EXISTS ui_commands (plugin TEXT PRIMARY KEY, commands TEXT NOT NULL);
`

var commandID = regexp.MustCompile(`^[a-z0-9_]+$`)

type paletteCommand struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	Route       string   `json:"route"`
	Surface     string   `json:"surface,omitempty"` // "form" (default) or "panel" (panels.go)
	Landing     bool     `json:"landing,omitempty"` // the panel a Home click opens; one per plugin
	// Agent parity (docs/testing.md): the exported agent action that does what the command's form
	// does, or the reason that only the user can do it.
	Action   string `json:"action,omitempty"`
	UserOnly string `json:"user_only,omitempty"`
}

type formContext struct {
	SessionID    string `json:"session_id"`
	WindowID     string `json:"window_id"`
	SourceViewID string `json:"source_view_id"`
	SourceRoute  string `json:"source_route"`
	SourceBlock  string `json:"source_block_id,omitempty"` // the pane the command ran from (for terminal.pwd, placement)
	// SourcePwd is the raw terminal.pwd value of SourceBlock, read once at the open; SourcePwdError
	// is why that read gave no value. Neither is set without a source pane. A render reads them and
	// never writes (decision "view render never writes; freeze open state in the form context").
	SourcePwd      string       `json:"source_pwd,omitempty"`
	SourcePwdError string       `json:"source_pwd_error,omitempty"`
	Panel          bool         `json:"panel,omitempty"`   // a side panel, not a form (panels.go)
	Return         *slotReturn  `json:"return,omitempty"`  // the panel this form replaced
	Restart        *restartForm `json:"restart,omitempty"` // the hub's restart form: its snapshot (restartform.go)
	// UnderBlock (ui_slot "layer") is the window's focused tiled block before the slot's first
	// layer opened; a replacement keeps it. A closed form's return takes the focus only from it.
	UnderBlock string `json:"under_block_id,omitempty"`
}

func formsStart(ctx context.Context, db *DB) error {
	if _, err := db.ExecContext(ctx, formSchema); err != nil {
		return err
	}
	for _, m := range []string{
		`ALTER TABLE views ADD COLUMN context TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.ExecContext(ctx, m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// parseCommands checks the commands of a hub.register call. A missing field means no commands.
func parseCommands(plugin string, v any) ([]paletteCommand, error) {
	if v == nil {
		return nil, nil
	}
	raw, _ := json.Marshal(v)
	if string(raw) == "{}" { // Rex Lua encodes an empty table as {}
		return nil, nil
	}
	var cmds []paletteCommand
	if err := json.Unmarshal(raw, &cmds); err != nil {
		return nil, bad("commands must be a list of {id, title, route}")
	}
	if len(cmds) > 0 && !commandID.MatchString(plugin) {
		return nil, bad("plugin name %q cannot name a palette action (a-z, 0-9, _)", plugin)
	}
	seen := map[string]bool{}
	landing := false
	for i, c := range cmds {
		switch {
		case !commandID.MatchString(c.ID):
			return nil, bad("command id %q: use a-z, 0-9 and _", c.ID)
		case seen[c.ID]:
			return nil, bad("command id %q is declared twice", c.ID)
		case strings.TrimSpace(c.Title) == "":
			return nil, bad("command %q has no title", c.ID)
		}
		seen[c.ID] = true
		switch c.Surface {
		case "", "form":
			cmds[i].Surface = "" // the default is stored as empty
			if c.Landing {
				return nil, bad("command %q: only a panel can be the landing", c.ID)
			}
			if !strings.HasPrefix(c.Route, "/forms/") {
				return nil, bad("command %q: route must start with /forms/", c.ID)
			}
		case "panel":
			if !strings.HasPrefix(c.Route, "/panels/") {
				return nil, bad("command %q: a panel route must start with /panels/", c.ID)
			}
			if c.Landing && landing {
				return nil, bad("command %q: only one panel may be the landing", c.ID)
			}
			landing = landing || c.Landing
		default:
			return nil, bad("command %q: surface must be form or panel", c.ID)
		}
	}
	return cmds, nil
}

// saveCommands records a plugin's commands and rewrites the palette file when they changed. No row
// is an empty stored set; any other read or write error is returned.
func (h *Hub) saveCommands(ctx context.Context, plugin string, cmds []paletteCommand) error {
	b, _ := json.Marshal(cmds)
	if cmds == nil {
		b = []byte("[]")
	}
	var old string
	err := h.db.QueryRowContext(ctx, `SELECT commands FROM ui_commands WHERE plugin = ?`, plugin).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if old == string(b) {
		return nil
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO ui_commands (plugin, commands) VALUES (?, ?)
		ON CONFLICT (plugin) DO UPDATE SET commands = excluded.commands`, plugin, string(b)); err != nil {
		return err
	}
	h.kickPalette()
	return nil
}

func (h *Hub) kickPalette() {
	if h.paletteKick == nil {
		return
	}
	select {
	case h.paletteKick <- struct{}{}:
	default:
	}
}

// The bounds of paletteStop (docs/hub.md, "Stop and exit status"): the wait for the palette loop,
// and the config reload after the stop file. They are separate bounds: file and process work can
// add to them.
const (
	paletteStopJoin   = 2 * time.Second
	paletteStopReload = 3 * time.Second
)

// startPalette starts paletteLoop on a context of its own, so that paletteStop can end it, and
// asks for a first write.
func (h *Hub) startPalette() {
	ctx, cancel := context.WithCancel(context.Background())
	h.paletteCancel, h.paletteDone, h.paletteTried = cancel, make(chan struct{}), make(chan struct{})
	go h.paletteLoop(ctx)
	h.kickPalette()
}

// paletteLoop writes the palette file when its content changes, then reloads the Rex config.
// A failed write or reload retries after a few seconds. It returns when paletteStop cancels ctx.
func (h *Hub) paletteLoop(ctx context.Context) {
	defer close(h.paletteDone)
	var retry <-chan time.Time
	tried := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.paletteKick:
		case <-retry:
		}
		if ctx.Err() != nil { // a select can take a ready kick when the cancel is ready too
			return
		}
		retry = nil
		err := h.writePalette(ctx)
		if !tried {
			tried = true
			close(h.paletteTried)
		}
		if ctx.Err() != nil {
			return // the stop owns the file now
		}
		if err != nil {
			log.Printf("palette: %v", err)
			retry = time.After(5 * time.Second)
		}
	}
}

// paletteStop runs once, on each orderly exit after startPalette: it ends paletteLoop and then
// replaces the file with paletteHeader alone, which declares no entry, and reloads the Rex config.
// So no hub or plugin entry stays in the palette after the hub ends; the boot entries of setup
// (lazyrex_boot.lua) stay. It writes only a file that still holds what this hub put there. A crash
// cannot run it: the next start writes the file again (writePalette).
func (h *Hub) paletteStop() {
	h.paletteOnce.Do(func() {
		if h.paletteCancel == nil {
			return
		}
		h.paletteCancel()
		select {
		case <-h.paletteDone:
		case <-time.After(paletteStopJoin):
			log.Print("palette: the palette write did not end; not rewritten at stop")
			return
		}
		path := h.cfg.PaletteLua
		if path == "" || h.paletteMine == "" {
			return // this hub never wrote or found its file
		}
		if old, err := os.ReadFile(path); err != nil {
			log.Printf("palette: %v; not rewritten at stop", err)
			return
		} else if string(old) != h.paletteMine {
			log.Print("palette: another writer changed the file; not rewritten at stop")
			return
		}
		ctx := context.Background() // the hub's context is already cancelled
		if err := h.publishPalette(ctx, path, paletteHeader); err != nil {
			log.Printf("palette: %v; not rewritten at stop", err)
			return
		}
		rctx, cancel := context.WithTimeout(ctx, paletteStopReload)
		defer cancel()
		out, err := h.reloadRexConfig(rctx)
		h.ev.emit("palette_written", map[string]any{"path": path, "commands": 0, "reload_ok": err == nil, "reload": out, "stop": true})
		if err != nil {
			log.Printf("palette: rex config reload at stop: %v: %s", err, out)
			return
		}
		h.paletteLoaded = paletteHeader
		if _, err := h.db.ExecContext(ctx, `INSERT INTO ui_state (key, value) VALUES ('palette_loaded', ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, paletteHeader); err != nil {
			log.Printf("palette: %v", err)
		}
	})
}

// publishPalette replaces the file at path with content: a temporary file in the same directory,
// then a rename. A failure keeps the old file. A cancelled ctx publishes nothing: paletteStop owns
// the file after its cancel.
func (h *Hub) publishPalette(ctx context.Context, path, content string) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	if h.paletteHook != nil {
		h.paletteHook()
	}
	if err := ctx.Err(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// reloadRexConfig runs `rex config reload`. A cancelled ctx kills rex; WaitDelay bounds the wait
// for output pipes that a child of rex could still hold.
func (h *Hub) reloadRexConfig(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, h.rexBinary, "-S", h.cfg.Server, "--autostart=false", "config", "reload")
	cmd.Env = append(os.Environ(), "REX_SERVER="+h.cfg.Server)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (h *Hub) writePalette(ctx context.Context) error {
	path := h.cfg.PaletteLua
	if path == "" {
		return nil
	}
	cmds := map[string][]paletteCommand{}
	for _, name := range h.pluginNames() {
		var raw string
		if err := h.db.QueryRowContext(ctx, `SELECT commands FROM ui_commands WHERE plugin = ?`, name).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		var list []paletteCommand
		if json.Unmarshal([]byte(raw), &list) == nil && len(list) > 0 {
			cmds[name] = list
		}
	}
	content := paletteLua(cmds)
	if h.paletteLoaded == "" {
		// The content of the last successful reload, recorded after it: equal bytes on disk alone
		// do not prove a reload (a crash between the rename and the reload).
		h.db.QueryRowContext(ctx, `SELECT value FROM ui_state WHERE key = 'palette_loaded'`).Scan(&h.paletteLoaded)
	}
	// Skip only when the last reload had this content and the file still holds it: a stop file
	// whose reload failed leaves palette_loaded at the full content.
	old, err := os.ReadFile(path)
	onDisk := err == nil && string(old) == content
	if content == h.paletteLoaded && onDisk {
		h.paletteMine = content
		return nil
	}
	if !onDisk {
		if err := h.publishPalette(ctx, path, content); err != nil {
			return err
		}
	}
	h.paletteMine = content
	if err := ctx.Err(); err != nil {
		return err // paletteStop reloads after its own write
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := h.reloadRexConfig(cctx)
	h.ev.emit("palette_written", map[string]any{"path": path, "commands": len(cmds), "reload_ok": err == nil, "reload": out})
	if err != nil {
		return fmt.Errorf("rex config reload: %v: %s", err, out)
	}
	h.paletteLoaded = content
	_, err = h.db.ExecContext(ctx, `INSERT INTO ui_state (key, value) VALUES ('palette_loaded', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, content)
	return err
}

// luaString quotes s as a Lua 5.1 string literal.
func luaString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%03d", c)
		default:
			b.WriteByte(c) // UTF-8 bytes pass through
		}
	}
	b.WriteByte('"')
	return b.String()
}

// paletteHeader begins every palette file. Alone, it is the stop file (paletteStop): a module that
// declares no entry.
const paletteHeader = `-- Generated by the LazyRex hub. Do not edit: the hub replaces this file.
-- Load it from init.lua with: pcall(require, "lazyrex_palette")
`

// paletteLua is the generated Lua file. Its content depends only on the commands, sorted, so an
// unchanged command set gives the same bytes.
func paletteLua(cmds map[string][]paletteCommand) string {
	var b strings.Builder
	b.WriteString(paletteHeader)
	b.WriteString(`local function lazyrex_hub()
  local list, err = rex.call("client.list", {})
  if err ~= nil then error(err, 0) end
  local hub
  for _, c in ipairs(list.clients or {}) do
    if c.info and c.info.label == "lazyrex-hub" and c.connection_state == "connected" then
      if hub then error("more than one LazyRex hub is connected", 0) end
      hub = c.client_id
    end
  end
  if not hub then error("the LazyRex hub is not running", 0) end
  return hub
end
local function lazyrex_run(command)
  return function(ctx, args)
    local r, perr = rex.call("client.perform", { client_id = lazyrex_hub(), action = "hub.palette.run",
      args = { command = command, ctx = ctx }, timeout_ms = 10000 })
    if perr ~= nil then error(perr, 0) end
    local res = (r and r.result) or {}
    if res.select then pcall(rex.client.queue, "session.select", res.select) end
    return res
  end
end
-- lazyrex_admin runs a hub admin action. The key is new each second: a second run in the same
-- second replays the first one's operation.
local function lazyrex_admin(action, name, args)
  return function(ctx, _)
    local a = { key = "palette-" .. name .. "-" .. os.time() }
    for k, v in pairs(args) do a[k] = v end
    local r, perr = rex.call("client.perform", { client_id = lazyrex_hub(), action = action, args = a, timeout_ms = 10000 })
    if perr ~= nil then error(perr, 0) end
    return (r and r.result) or {}
  end
end
`)
	b.WriteString(hubPaletteLua)

	plugins := make([]string, 0, len(cmds))
	for p := range cmds {
		plugins = append(plugins, p)
	}
	sort.Strings(plugins)
	for _, p := range plugins {
		list := append([]paletteCommand(nil), cmds[p]...)
		sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
		for _, c := range list {
			fmt.Fprintf(&b, "\nrex.action {\n  name = %s,\n  title = %s,\n  category = \"LazyRex\",\n",
				luaString("lazyrex_"+p+"_"+c.ID), luaString("LazyRex: "+c.Title))
			if c.Description != "" {
				fmt.Fprintf(&b, "  description = %s,\n", luaString(c.Description))
			}
			if len(c.Keywords) > 0 {
				// Rex 1203 takes keywords as one string (a table fails the config load).
				fmt.Fprintf(&b, "  keywords = %s,\n", luaString(strings.Join(c.Keywords, " ")))
			}
			fmt.Fprintf(&b, "  run = lazyrex_run(%s),\n}\n", luaString(p+"."+c.ID))
		}
	}
	return b.String()
}

// paletteRun handles hub.palette.run {command: "<plugin>.<id>", ctx}: it opens the command's form
// in the window of ctx.block_id.
func (h *Hub) paletteRun(ctx context.Context, p perform, args map[string]any) (any, error) {
	command, _ := args["command"].(string)
	plugin, id, ok := strings.Cut(command, ".")
	if !ok {
		return nil, bad("unknown command %q", command)
	}
	var raw string
	if err := h.db.QueryRowContext(ctx, `SELECT commands FROM ui_commands WHERE plugin = ?`, plugin).Scan(&raw); err != nil {
		return nil, bad("unknown command %q", command)
	}
	var list []paletteCommand
	json.Unmarshal([]byte(raw), &list)
	var route, title string
	panel := false
	for _, c := range list {
		if c.ID == id {
			route, title, panel = c.Route, c.Title, c.Surface == "panel"
		}
	}
	if route == "" {
		return nil, bad("unknown command %q", command)
	}
	if h.pluginClient(plugin) == "" {
		return nil, bad("plugin %s is not running", plugin)
	}
	rc, _ := args["ctx"].(map[string]any)
	// A config action's client.perform comes from the client that ran it (observed on Rex 1203),
	// and Rex sets ctx.client_id to that client. A ctx from anyone else is not this run's.
	if cid, _ := rc["client_id"].(string); cid == "" || cid != p.Requester.ClientID {
		return nil, bad("ctx.client_id is not the calling client")
	}
	sess, _ := rc["session_id"].(string)
	block, _ := rc["block_id"].(string)
	if sess == "" {
		return nil, bad("run this command from the Rex palette or a key binding: it needs a session")
	}
	window, err := h.formWindow(ctx, sess, block)
	if err != nil {
		return nil, err
	}
	fc := formContext{SessionID: sess, WindowID: window, Panel: panel, SourceBlock: block}
	if src, err := h.viewByBlock(ctx, block); err == nil && src != nil && src.Tab != formTab {
		fc.SourceViewID, fc.SourceRoute = src.ID, src.Route
	} else if err == nil && src != nil {
		// Run from a side panel or form: the source is that view's own source, not the side pane
		// (its cwd is the renderer's, and the replacement closes it).
		if prev, ok := h.formOf(ctx, src.ID); ok {
			fc.SourceViewID, fc.SourceRoute, fc.SourceBlock = prev.SourceViewID, prev.SourceRoute, prev.SourceBlock
		}
	}
	h.freezeSourcePwd(ctx, &fc)
	// The form or panel takes the window's side slot; a view there is replaced (panels.go).
	out, err := h.openSlot(ctx, slotRequest{Plugin: plugin, Route: route, Ctx: fc, Focus: true, NextTitle: title})
	if err != nil {
		return nil, err
	}
	if out["status"] == "opened" {
		go h.refocusForm(sess, window, h.viewBlock(out["view_id"].(string)), block)
	}
	out["select"] = map[string]any{"session_id": sess, "window_id": window}
	return out, nil
}

// formWindow checks that sess exists and returns the window of block, or the active window when
// block is empty. A block that is not in sess is an error.
func (h *Hub) formWindow(ctx context.Context, sess, block string) (string, error) {
	if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
		return "", bad("session %s: %v", sess, err)
	}
	var sv struct {
		ActiveWindowID string `json:"active_window_id"`
		Windows        []struct {
			WindowID string `json:"window_id"`
			Layers   []struct {
				Blocks []struct {
					BlockID string `json:"block_id"`
				} `json:"blocks"`
			} `json:"layers"`
		} `json:"windows"`
	}
	if err := h.rex.Call(ctx, "session.view", map[string]any{"session_id": sess}, &sv); err != nil {
		return "", err
	}
	if block == "" {
		if sv.ActiveWindowID == "" {
			return "", bad("session %s has no window", sess)
		}
		return sv.ActiveWindowID, nil
	}
	for _, w := range sv.Windows {
		for _, l := range w.Layers {
			for _, b := range l.Blocks {
				if b.BlockID == block {
					return w.WindowID, nil
				}
			}
		}
	}
	return "", bad("block %s is not shown in session %s", block, sess)
}

// errUnknown marks an outcome that Rex did not report (a timeout or a lost connection). The step
// runs again later; it must be safe to repeat.
var errUnknown = errors.New("outcome unknown")

// transient reports whether err may succeed on a later try.
func transient(err error) bool {
	if errors.Is(err, errUnknown) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var re *rex.RexError
	if errors.As(err, &re) {
		_, definite := definiteFailure(err)
		return !definite
	}
	return false
}

// refocusForm gives the new form focus again after a palette run. A workaround: Rex focuses the
// form, but in the app the form did not have focus (the maintainer, 2026-10-17). Inferred: the app gives
// focus back to the pane where the palette ran when the palette closes. The hub moves focus only
// from that pane (anchor) in the active window, so a user who moved to another pane or tab keeps it.
func (h *Hub) refocusForm(sess, window, block, anchor string) {
	for _, wait := range []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second} {
		time.Sleep(wait)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var sv struct {
			ActiveWindowID string `json:"active_window_id"`
			Windows        []struct {
				WindowID       string `json:"window_id"`
				FocusedBlockID string `json:"focused_block_id"`
			} `json:"windows"`
		}
		err := h.rex.Call(ctx, "session.view", map[string]any{"session_id": sess}, &sv)
		focused := ""
		for _, w := range sv.Windows {
			if w.WindowID == window {
				focused = w.FocusedBlockID
			}
		}
		if sv.ActiveWindowID != window {
			focused = "" // the user is on another tab: focus_block would pull them back
		}
		if err == nil && anchor != "" && focused == anchor {
			if err = h.rex.Call(ctx, "session.focus_block", map[string]any{"session_id": sess, "block_id": block}, nil); err == nil {
				h.ev.emit("form_refocused", map[string]any{"block_id": block, "after_ms": wait.Milliseconds()})
			}
		}
		cancel()
		if err != nil || focused != block && focused != anchor {
			return // gone, or the user is elsewhere
		}
	}
}

func (h *Hub) adoptForm(ctx context.Context, v *view, fc formContext, block string) error {
	if err := h.adoptView(ctx, v, fc.SessionID, rexBlock{BlockID: block, WindowID: fc.WindowID}); err != nil {
		return fmt.Errorf("adopt form %s: %v: %w", v.ID, err, errUnknown)
	}
	h.ev.emit("form_opened", map[string]any{"plugin": v.Plugin, "view_id": v.ID, "route": v.Route, "session_id": fc.SessionID, "window_id": fc.WindowID, "source_view_id": fc.SourceViewID})
	return nil
}

// findFormBlock finds the pane of form id in sess by its unique label. An error means the
// inventory is incomplete: found=false then proves nothing.
func (h *Hub) findFormBlock(ctx context.Context, sess, id string) (string, bool, error) {
	if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
		if sessionGone(err) {
			return "", false, nil
		}
		return "", false, err
	}
	blocks, err := h.blocksIn(ctx, sess)
	if err != nil {
		if sessionGone(err) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, b := range blocks {
		if b.Label == "lazyrex-form:"+id {
			return b.BlockID, true, nil
		}
	}
	return "", false, nil
}

// closeForm closes a form's pane. The hub marks its own close, so the event is not the user's.
// A failed close keeps the row: the pane may still run. Closing a closed form does nothing.
func (h *Hub) closeForm(ctx context.Context, id, why string) error {
	var sess, block string
	if err := h.db.QueryRowContext(ctx, `SELECT session_id, block_id FROM views WHERE view_id = ? AND tab = ? AND status != 'closed'`, id, formTab).
		Scan(&sess, &block); err != nil {
		return nil
	}
	if block != "" {
		h.mu.Lock()
		h.selfClosed[block] = true
		h.mu.Unlock()
		if err := h.rex.Call(ctx, "block.close", map[string]any{"session_id": sess, "block_id": block}, nil); err != nil {
			var re *rex.RexError
			if !sessionGone(err) && !(errors.As(err, &re) && re.Code == "not_found") {
				h.mu.Lock()
				delete(h.selfClosed, block)
				h.mu.Unlock()
				return fmt.Errorf("close form %s: %v: %w", id, err, errUnknown)
			}
		}
	}
	// The row closes only after Rex closed the pane, so a failed close can be tried again.
	won, err := h.closeViewOnce(ctx, id, "hub", false)
	if err != nil {
		return fmt.Errorf("close form %s: %w", id, err)
	}
	if won {
		h.ev.emit("form_closed", map[string]any{"view_id": id, "by": "hub", "why": why})
	}
	return nil
}

// formOf returns the stored context of view id, and whether the view is a form.
func (h *Hub) formOf(ctx context.Context, id string) (formContext, bool) {
	var tabKey, raw string
	var fc formContext
	if h.db.QueryRowContext(ctx, `SELECT tab, context FROM views WHERE view_id = ?`, id).Scan(&tabKey, &raw) != nil || tabKey != formTab {
		return fc, false
	}
	json.Unmarshal([]byte(raw), &fc)
	return fc, true
}

// viewEffects are the UI parts of a view action result. They run after the operation commits.
type viewEffects struct {
	Navigate   navTarget      `json:"navigate"`
	Form       string         `json:"form"`
	CloseForm  bool           `json:"close_form"`
	Panel      map[string]any `json:"panel"`       // {command} | {route} | {plugin, command} (panels.go)
	ClosePanel bool           `json:"close_panel"` // closes the panel that ran the action
}

func (e viewEffects) any() bool {
	return e.Navigate.set || e.Form != "" || e.CloseForm || e.Panel != nil || e.ClosePanel
}

// navTarget is a navigate effect: a tab key of the plugin that ran the action ("thread/a"), or
// {plugin, tab} for a tab of another plugin (the home plugin opens other plugins' tabs). The
// navigation then runs in the view worker and its outcome is the effect's outcome.
type navTarget struct {
	Plugin, Tab string
	set         bool
}

func (n *navTarget) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "null" {
		return nil // no navigation
	}
	n.set = true // present: from here on, a bad target is a failed effect, never a no-op
	var key string
	if json.Unmarshal(b, &key) == nil {
		n.Tab = key
		return nil
	}
	var t struct {
		Plugin *string `json:"plugin"`
		Tab    *string `json:"tab"`
	}
	if b = bytes.TrimSpace(b); len(b) == 0 || b[0] != '{' || json.Unmarshal(b, &t) != nil {
		return errBadNavigate
	}
	if t.Plugin != nil {
		n.Plugin = *t.Plugin
	}
	if t.Tab != nil {
		n.Tab = *t.Tab
	}
	return nil
}

var errBadNavigate = errors.New("navigate: the target must be a tab key or {plugin, tab} with string values")

// decodeEffects reads the effects of a view action result. A result that is not a JSON object
// has no effects (an empty Lua table can encode as []). An object with a malformed effect field
// returns the error: the effect must fail, not vanish.
func decodeEffects(result []byte) (viewEffects, error) {
	var e viewEffects
	b := bytes.TrimSpace(result)
	if len(b) == 0 || b[0] != '{' {
		return e, nil
	}
	if err := json.Unmarshal(b, &e); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) && ute.Field != "" {
			return e, fmt.Errorf("effects: field %s: wrong type %s", ute.Field, ute.Value)
		}
		return e, fmt.Errorf("effects: %w", err)
	}
	return e, nil
}

// applyEffects runs the effects of one succeeded view action, in order: navigate, open a form,
// then close the form that ran the action. Progress is kept in view_ops (effect_state 'pending',
// effect_nav), so a retry or a hub restart goes on from the last finished step and repeats no
// step that is unsafe to repeat. The new form's view ID comes from the operation key, so opening
// it again finds it. A failure that a retry cannot fix ends as effect_state 'failed' with the
// error, which the renderer gets from hub.view.operation; the form that ran the action stays
// open. It returns true when a step must run again later.
func (h *Hub) applyEffects(ctx context.Context, plugin, key, viewID string, result json.RawMessage) bool {
	e, decodeErr := decodeEffects(result)
	var navDone int
	if err := h.db.QueryRowContext(ctx, `SELECT effect_nav FROM view_ops WHERE plugin = ? AND user = ? AND op_key = ?`, plugin, viewUser, key).Scan(&navDone); err != nil {
		log.Printf("views %s: effects of %s: %v (retry)", plugin, key, err)
		return true
	}
	set := func(query string, a ...any) bool { // reports whether the progress write failed
		_, err := h.db.ExecContext(ctx, query, a...)
		if err != nil {
			log.Printf("views %s: effects of %s: record: %v (retry)", plugin, key, err)
		}
		return err != nil
	}
	fail := func(err error) bool {
		if transient(err) {
			log.Printf("views %s: effects of %s: %v (retry)", plugin, key, err)
			return true
		}
		if set(`UPDATE view_ops SET effect_state = 'failed', effect_error = ? WHERE plugin = ? AND user = ? AND op_key = ?`,
			err.Error(), plugin, viewUser, key) {
			return true
		}
		h.ev.emit("view_effect_failed", map[string]any{"plugin": plugin, "op_key": key, "view_id": viewID, "error": err.Error()})
		h.notifyView(plugin, viewID)
		return false
	}
	if decodeErr != nil {
		return fail(decodeErr)
	}
	if e.Navigate.set && navDone == 0 {
		target := e.Navigate.Plugin
		if target == "" {
			target = plugin
		}
		if e.Navigate.Tab == "" {
			return fail(fmt.Errorf("navigate: the effect names no tab"))
		}
		if err := h.navigateNow(ctx, target, e.Navigate.Tab); err != nil {
			return fail(fmt.Errorf("navigate %s/%s: %w", target, e.Navigate.Tab, err))
		}
		if set(`UPDATE view_ops SET effect_nav = 1 WHERE plugin = ? AND user = ? AND op_key = ?`, plugin, viewUser, key) {
			return true // navigation repeats safely
		}
	}
	fc, isForm := h.formOf(ctx, viewID)
	opened := false // a form or panel took the slot; it replaced the source view when it was there
	if e.Form != "" || e.Panel != nil {
		target, route, title, panel := plugin, e.Form, "", false
		if e.Panel != nil {
			t, r, err := h.panelTarget(ctx, plugin, e.Panel)
			if err != nil {
				return fail(fmt.Errorf("panel: %w", err))
			}
			target, route, panel = t, r, true
			title = h.commandTitle(ctx, t, r)
		} else if !strings.HasPrefix(e.Form, "/forms/") {
			return fail(fmt.Errorf("form %q: the route must start with /forms/", e.Form))
		}
		if !isForm {
			// The window comes from Rex, not from the views row: the hub moves tabs between
			// windows (detach and restore), and the row can lag behind.
			var src view
			if err := h.db.QueryRowContext(ctx, `SELECT session_id, block_id, route FROM views WHERE view_id = ? AND status = 'open'`, viewID).
				Scan(&src.Session, &src.Block, &src.Route); err != nil {
				return fail(fmt.Errorf("form %s: the source view is closed", e.Form))
			}
			window, err := h.formWindow(ctx, src.Session, src.Block)
			if err != nil {
				return fail(fmt.Errorf("form %s: the source view is not shown: %w", e.Form, err))
			}
			fc = formContext{SessionID: src.Session, WindowID: window, SourceViewID: viewID, SourceRoute: src.Route, SourceBlock: src.Block}
			h.freezeSourcePwd(ctx, &fc)
		}
		sc := fc
		sc.Panel, sc.Return = panel, nil
		// The new view takes the source form's place. The source is still asked to release, so
		// input typed after the submit is not lost.
		req := slotRequest{ReplID: formEffectID(plugin, key), Plugin: target, Route: route, Ctx: sc, Focus: true, NextTitle: title}
		if isForm && !fc.Panel {
			req.ExpectOld = viewID
		}
		if _, err := h.openSlot(ctx, req); err != nil {
			return fail(err)
		}
		opened = true
	}
	switch {
	case opened:
	case isForm && fc.Panel && (e.ClosePanel || e.CloseForm):
		if err := h.closeForm(ctx, viewID, "action"); err != nil {
			return fail(err)
		}
	case isForm && !fc.Panel && e.any():
		// The form ended: the panel it replaced takes its place again (panels.go).
		// The form ended. The panel it replaced takes its place again (panels.go); the replacement
		// closes the form after the release, so a dirty form is not closed while the user decides.
		if took, err := h.formEnded(ctx, viewID, fc, true, key); err != nil {
			return fail(fmt.Errorf("return panel: %w", err)) // the form stays open
		} else if !took {
			if err := h.closeForm(ctx, viewID, "action"); err != nil {
				return fail(err)
			}
		}
	}
	if set(`UPDATE view_ops SET effect_state = 'done' WHERE plugin = ? AND user = ? AND op_key = ?`, plugin, viewUser, key) {
		return true // every step repeats safely
	}
	h.notifyView(plugin, viewID)
	return false
}

// formEffectID is the view ID of the form that operation key opens.
func formEffectID(plugin, key string) string {
	sum := sha256.Sum256([]byte(plugin + "\x00" + key))
	return "view-form-" + hex.EncodeToString(sum[:8])
}

func (h *Hub) notifyView(plugin, viewID string) {
	h.vs.mu.Lock()
	h.vs.notify[viewID] = true
	h.vs.stateLocked(plugin, viewID).dirty |= causeOperation
	h.vs.dirty[plugin] = true
	h.vs.mu.Unlock()
	h.kickViews(plugin, 0)
}

// formsSweep runs at start. A form whose pane request had an unknown outcome is looked up by
// its label: found, it is adopted; proved absent, it is abandoned. A form whose pane closed while
// the hub was down is closed. Effects that a crash cut off run again from view_ops (runOps).
func (h *Hub) formsSweep(ctx context.Context) {
	rows, err := h.db.QueryContext(ctx, `SELECT view_id, plugin, route, status, session_id, block_id, context FROM views WHERE tab = ? AND status != 'closed'`, formTab)
	if err != nil {
		return
	}
	type form struct{ id, plugin, route, status, sess, block, context string }
	var forms []form
	for rows.Next() {
		var f form
		rows.Scan(&f.id, &f.plugin, &f.route, &f.status, &f.sess, &f.block, &f.context)
		forms = append(forms, f)
	}
	rows.Close()
	for _, f := range forms {
		if f.status == "sending" && h.isSlotView(ctx, f.id) {
			continue // a side-slot pane: slotSweep looks for it and never abandons it on an inventory
		}
		var fc formContext
		json.Unmarshal([]byte(f.context), &fc)
		if f.sess == "" {
			f.sess = fc.SessionID
		}
		block, found, err := h.findFormBlock(ctx, f.sess, f.id)
		if err != nil {
			continue // incomplete inventory: keep the row as it is
		}
		switch {
		case found && f.status == "sending":
			if err := h.adoptForm(ctx, &view{ID: f.id, Plugin: f.plugin, Tab: formTab, Route: f.route}, fc, block); err != nil {
				log.Printf("form sweep %s: %v", f.id, err)
			}
		case found:
		case f.status == "sending":
			h.closeViewRow(ctx, f.id, "abandoned")
		default:
			h.closeViewRow(ctx, f.id, "gone")
		}
	}
}

// hubPaletteLua is the hub's own palette entries (docs/hub.md, "Agent resume" and "Agent
// restart"). Resume runs without a confirmation: it stops nothing. A restart entry opens the hub's
// restart form (restartform.go), which lists the agents first.

const hubPaletteLua = `
rex.action {
  name = "lazyrexhub_resume_detached",
  title = "LazyRex: Resume detached agents",
  category = "LazyRex",
  description = "Resume every detached agent that has a spawn record, each in a new pane",
  keywords = "agent resume detached",
  run = lazyrex_admin("hub.admin.resume", "resume", {}),
}
local function lazyrex_restart_form(kind)
  return function(ctx, _)
    local r, perr = rex.call("client.perform", { client_id = lazyrex_hub(), action = "hub.admin.restart.form",
      args = { kind = kind, ctx = ctx }, timeout_ms = 10000 })
    if perr ~= nil then error(perr, 0) end
    local res = (r and r.result) or {}
    if res.select then pcall(rex.client.queue, "session.select", res.select) end
    return res
  end
end
rex.action {
  name = "lazyrexhub_restart_codex",
  title = "LazyRex: Restart Codex agents…",
  category = "LazyRex",
  description = "List the current Codex agents, then stop and resume them in new panes",
  keywords = "agent restart codex",
  run = lazyrex_restart_form("codex"),
}
rex.action {
  name = "lazyrexhub_restart_claude",
  title = "LazyRex: Restart Claude agents…",
  category = "LazyRex",
  description = "List the current Claude agents, then stop and resume them in new panes",
  keywords = "agent restart claude",
  run = lazyrex_restart_form("claude"),
}
`

// freezeSourcePwd reads the working directory of fc's source pane once, at the open, and keeps the
// raw value (the plugin decodes it). A form that a form opens keeps its source's frozen value.
func (h *Hub) freezeSourcePwd(ctx context.Context, fc *formContext) {
	fc.SourcePwd, fc.SourcePwdError = "", ""
	if fc.SourceBlock == "" {
		return
	}
	var out struct {
		Pwd string `json:"pwd"`
	}
	if err := h.rex.BlockCall(ctx, "com.superlogical.terminal.pwd", fc.SessionID, fc.SourceBlock, map[string]any{}, &out); err != nil {
		fc.SourcePwdError = "the source pane's directory could not be read: " + err.Error()
		return
	}
	if out.Pwd == "" {
		fc.SourcePwdError = "This pane did not report its folder. Enter a directory, or leave it empty for each profile's own directory."
		return
	}
	fc.SourcePwd = out.Pwd
}

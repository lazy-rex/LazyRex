package main

// Profiles and workers: named commands that the hub starts in a new tab of its session.
// An agent profile also creates a hub agent for the new pane, so the agent can join and
// message without a hand-written roster entry. See README "Profiles and workers".

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

const workersSchema = `
-- One row per hub.spawn request. status: prepared (admitted, no create request sent yet), sending
-- (the create request may have been sent: outcome unknown until resolved), open, failed, closed.
-- generation is the hub generation that admitted the row: a prepared row of an older generation
-- was never sent.
-- request is the exact request content; a retry with the same key must match it.
CREATE TABLE IF NOT EXISTS spawns (spawn_id TEXT PRIMARY KEY, caller TEXT NOT NULL, key TEXT NOT NULL,
  request TEXT NOT NULL,
  profile TEXT NOT NULL, title TEXT NOT NULL, agent_id TEXT NOT NULL DEFAULT '', parent TEXT NOT NULL,
  session_id TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, window_id TEXT NOT NULL DEFAULT '', block_id TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '', closed_by TEXT NOT NULL DEFAULT '', created TEXT NOT NULL,
  effective_args TEXT NOT NULL DEFAULT '',
  generation INTEGER NOT NULL DEFAULT 0,
  UNIQUE (caller, key));
-- Agents that a spawn created. IDs are never reused: the mailbox history belongs to one instance.
CREATE TABLE IF NOT EXISTS dyn_agents (agent TEXT PRIMARY KEY, name TEXT NOT NULL, profile TEXT NOT NULL,
  spawn_id TEXT NOT NULL, created TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS counters (name TEXT PRIMARY KEY, value INTEGER NOT NULL);
-- A locked path refuses new spawns whose cwd is the path or under it (hub.cwd.lock), for example
-- while a plugin removes a worktree. owner is the plugin; key is its operation key.
CREATE TABLE IF NOT EXISTS cwd_locks (path TEXT PRIMARY KEY, owner TEXT NOT NULL, key TEXT NOT NULL, created TEXT NOT NULL);
`

// Profile is one named command. Later files replace a profile with the same id.
type Profile struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Kind    string            `json:"kind"` // agent or accessory
	Command []string          `json:"command"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Args    []ProfileArg      `json:"args,omitempty"`
	// Resume is the command that continues a conversation (agent profiles only): ${conversation_id}
	// is the identity's conversation, other tokens are the spawn's args (resume.go).
	Resume []string `json:"resume,omitempty"`
}

// ProfileArg is a named value that a spawn can pass. "${name}" in the command is replaced by the
// value, as one whole argument (no shell, no quoting). Without a value or default, that argument
// is left out, so "claude ${task}" runs plain "claude".
type ProfileArg struct {
	Name     string `json:"name"`
	Label    string `json:"label,omitempty"`
	Default  string `json:"default,omitempty"`
	Required bool   `json:"required,omitempty"`
}

var argToken = regexp.MustCompile(`\$\{([^}]*)\}`)

var profileID = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// profileFiles returns the profile files in override order. Global first; spawn points later.
func (h *Hub) profileFiles() []string {
	if h.cfg.Profiles == "" {
		return nil
	}
	return []string{h.cfg.Profiles}
}

// loadProfiles reads every profile file. A missing file is empty; an invalid file is an error,
// so a typo never silently removes a profile.
func (h *Hub) loadProfiles() (map[string]Profile, []string, error) {
	return h.loadFrom(h.profileFiles())
}

// builtinShell is the plain login shell: no agent, no messaging. The Templates pair starts it too.
// A profile file can replace it with its own "shell" profile.
func builtinShell() Profile {
	sh := os.Getenv("SHELL")
	if !filepath.IsAbs(sh) {
		sh = "/bin/zsh"
	}
	return Profile{ID: "shell", Name: "Shell", Kind: "accessory", Command: []string{sh, "-l"}}
}

func (h *Hub) loadFrom(files []string) (map[string]Profile, []string, error) {
	byID := map[string]Profile{"shell": builtinShell()}
	order := []string{"shell"}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		var f struct {
			Profiles []Profile `json:"profiles"`
		}
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		seen := map[string]bool{}
		for i, p := range f.Profiles {
			if err := validProfile(p); err != nil {
				return nil, nil, fmt.Errorf("%s: profile %d: %w", path, i, err)
			}
			if h.cfg.Test {
				if err := testProfileCheck(p, h.cfg.TestRoots); err != nil {
					return nil, nil, fmt.Errorf("%s: profile %d: %w", path, i, err)
				}
			}
			if seen[p.ID] {
				return nil, nil, fmt.Errorf("%s: profile %q appears twice", path, p.ID)
			}
			seen[p.ID] = true
			if _, ok := byID[p.ID]; !ok {
				order = append(order, p.ID)
			}
			byID[p.ID] = p
		}
	}
	return byID, order, nil
}

func validProfile(p Profile) error {
	if !profileID.MatchString(p.ID) {
		return fmt.Errorf("id %q must be lowercase letters, digits or _, at most 32", p.ID)
	}
	if p.Name == "" {
		return errors.New("name is required")
	}
	if p.Kind != "agent" && p.Kind != "accessory" {
		return fmt.Errorf("kind %q must be agent or accessory", p.Kind)
	}
	if len(p.Command) == 0 || p.Command[0] == "" {
		return errors.New("command is required")
	}
	if p.Cwd != "" && !filepath.IsAbs(p.Cwd) {
		return fmt.Errorf("cwd %q must be absolute", p.Cwd)
	}
	declared := map[string]bool{}
	for _, a := range p.Args {
		if !profileID.MatchString(a.Name) || declared[a.Name] {
			return fmt.Errorf("arg name %q must be unique lowercase letters, digits or _", a.Name)
		}
		declared[a.Name] = true
	}
	used := map[string]bool{}
	for i, a := range p.Command {
		for _, m := range argToken.FindAllStringSubmatch(a, -1) {
			if a != m[0] || i == 0 {
				return fmt.Errorf("%s must be a whole command argument, and not the program", m[0])
			}
			if !declared[m[1]] {
				return fmt.Errorf("%s is not a declared arg", m[0])
			}
			used[m[1]] = true
		}
	}
	for name := range declared {
		if !used[name] {
			return fmt.Errorf("arg %q is not used in the command", name)
		}
	}
	if err := validResume(p, declared); err != nil {
		return err
	}
	for k := range p.Env {
		if !envName.MatchString(k) || strings.HasPrefix(k, "LAZYREX_") {
			return fmt.Errorf("env %q is not allowed", k)
		}
	}
	return nil
}

func (h *Hub) workersSchemaStart(ctx context.Context) error {
	if _, err := h.db.ExecContext(ctx, workersSchema+identitySchema+spawnWindowSchema+grantsSchema); err != nil {
		return err
	}
	for _, m := range []string{
		`ALTER TABLE spawns ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE spawns ADD COLUMN cwd TEXT NOT NULL DEFAULT ''`,
		// The args after defaults, as JSON; '' on rows from before it was recorded (hub.spawn.spec).
		`ALTER TABLE spawns ADD COLUMN effective_args TEXT NOT NULL DEFAULT ''`,
		// The hub generation that admitted the row; 0 on rows from before the prepared state.
		`ALTER TABLE spawns ADD COLUMN generation INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := h.db.ExecContext(ctx, m); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

// workersLoad creates the schema and loads the dynamic agents. It runs before loadBindings, so a
// spawned agent keeps its binding across a hub restart.
func (h *Hub) workersLoad(ctx context.Context) error {
	if err := h.workersSchemaStart(ctx); err != nil {
		return err
	}
	if err := h.failUnsentSpawns(ctx); err != nil {
		return err
	}
	rows, err := h.db.QueryContext(ctx, `SELECT agent, name, profile FROM dyn_agents ORDER BY created, agent`)
	if err != nil {
		return err
	}
	defer rows.Close()
	h.mu.Lock()
	for rows.Next() {
		var id, name, profile string
		if err := rows.Scan(&id, &name, &profile); err != nil {
			h.mu.Unlock()
			return err
		}
		if h.agents[id] == nil {
			h.agents[id] = &Agent{ID: id, Name: name, Profile: profile, delivered: map[string]int64{}}
			h.agentOrder = append(h.agentOrder, id)
		}
	}
	h.mu.Unlock()
	return rows.Err()
}

// workersRun starts the spawn resolver and the profile watcher after initialization.
func (h *Hub) workersRun(ctx context.Context) {
	go h.resolveSpawns(ctx)
	h.checkProfiles() // the baseline
}

// spawnCaller is who asks: an agent (from its pane), the user (a plugin view action), or the agent
// of a forwarded call that a plugin handles now (on_behalf, agentface.go).
func (h *Hub) spawnCaller(ctx context.Context, p perform, args map[string]any) (string, error) {
	if raw, ok := args["on_behalf"]; ok {
		if _, ok := args["for"]; ok {
			return "", bad("on_behalf cannot be combined with for")
		}
		pid, ok := raw.(float64)
		if !ok || pid <= 0 || pid != math.Trunc(pid) || pid > math.MaxInt32 {
			return "", bad("on_behalf must be the requester_pid of a forwarded call")
		}
		if h.pluginByClient(p.Requester.ClientID) == nil {
			return "", bad("only a registered plugin may act on behalf of an agent")
		}
		if !h.forwarding(p.Requester.ClientID, int(pid)) {
			return "", bad("on_behalf names no agent call that the hub is forwarding to this plugin now")
		}
		return h.agentOfPID(ctx, int(pid))
	}
	if who, _ := args["for"].(string); who != "" {
		if who != "user" || h.pluginByClient(p.Requester.ClientID) == nil {
			return "", bad(`only a registered plugin may act for "user"`)
		}
		return "user", nil
	}
	return h.agentOfPID(ctx, p.Requester.Principal.PID)
}

// agentOfPID is the joined agent or current conversation identity of the pane that runs pid.
func (h *Hub) agentOfPID(ctx context.Context, pid int) (string, error) {
	_, block, _, err := h.paneOf(ctx, pid)
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// The pane's current conversation identity first: it needs no join.
	for _, list := range [][]*Agent{identsOf(h.idents), identsOf(h.agents)} {
		for _, a := range list {
			if block != "" && a.BlockID == block && a.live() && (a.Kind == "" || a.IdState == "current") {
				return a.ID, nil
			}
		}
	}
	return "", bad("the caller is not a joined agent or a reported conversation")
}

func (h *Hub) profilesList(ctx context.Context) (any, error) {
	byID, order, err := h.loadProfiles()
	if err != nil {
		return nil, err
	}
	running := map[string]int{}
	rows, err := h.db.QueryContext(ctx, `SELECT profile, COUNT(*) FROM spawns WHERE status IN ('prepared', 'sending', 'open') GROUP BY profile`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p string
		var n int
		rows.Scan(&p, &n)
		running[p] = n
	}
	rows.Close()
	list := []any{}
	for _, id := range order {
		p := byID[id]
		list = append(list, map[string]any{"id": p.ID, "name": p.Name, "kind": p.Kind, "args": profileArgs(p), "cwd": p.Cwd, "running": running[p.ID]})
	}
	return map[string]any{"profiles": list}, nil
}

type spawnRow struct {
	ID, Caller, Key, Profile, Title, Agent, Parent, Status, Window, Block, Error, ClosedBy, Created, Session string
	Cwd                                                                                                      string // the effective working directory ("" = the Rex default)
}

const spawnCols = `spawn_id, caller, key, profile, title, agent_id, parent, status, window_id, block_id, error, closed_by, created, session_id, cwd`

func scanSpawn(sc interface{ Scan(...any) error }, extra ...any) (*spawnRow, error) {
	var s spawnRow
	err := sc.Scan(append([]any{&s.ID, &s.Caller, &s.Key, &s.Profile, &s.Title, &s.Agent, &s.Parent, &s.Status, &s.Window, &s.Block, &s.Error, &s.ClosedBy, &s.Created, &s.Session, &s.Cwd}, extra...)...)
	return &s, err
}

func (s *spawnRow) result() map[string]any {
	status := s.Status
	if status == "prepared" || status == "sending" {
		status = "pending"
	}
	out := map[string]any{"spawn_id": s.ID, "status": status}
	if s.Session != "" {
		out["session_id"] = s.Session
	}
	if s.Block != "" {
		out["block_id"], out["window_id"] = s.Block, s.Window
	}
	if s.Agent != "" {
		out["agent_id"] = s.Agent
	}
	if s.Error != "" {
		out["error"] = s.Error
	}
	return out
}

// workerPlacement describes where Rex puts a new worker. Ratio is the new pane's share.
type workerPlacement struct {
	Session, Beside, Direction string
	Ratio                      float64
	Detached                   bool
	Focus                      *spawnFocus
}

func parseWorkerPlacement(raw any) (workerPlacement, error) {
	p := workerPlacement{Direction: "right", Ratio: 0.5}
	m, ok := raw.(map[string]any)
	if !ok {
		return p, bad("placement must be an object")
	}
	for k, v := range m {
		switch k {
		case "session", "beside", "direction":
			text, ok := v.(string)
			if !ok || text == "" {
				return p, bad("placement.%s must be nonempty text", k)
			}
			switch k {
			case "session":
				p.Session = text
			case "beside":
				p.Beside = text
			case "direction":
				p.Direction = text
			}
		case "ratio":
			n, ok := v.(float64)
			if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 || n >= 1 {
				return p, bad("placement.ratio must be greater than 0 and less than 1")
			}
			p.Ratio = n
		case "detached":
			b, ok := v.(bool)
			if !ok {
				return p, bad("placement.detached must be true or false")
			}
			p.Detached = b
		case "focus":
			var err error
			p.Focus, err = parseSpawnFocus(v)
			if err != nil {
				return p, err
			}
		default:
			return p, bad("unknown placement field %q", k)
		}
	}
	switch p.Direction {
	case "right", "left", "above", "below":
	default:
		return p, bad("placement.direction must be right, left, above or below")
	}
	if p.Beside == "" {
		if _, ok := m["direction"]; ok {
			return p, bad("placement.direction requires beside")
		}
		if _, ok := m["ratio"]; ok {
			return p, bad("placement.ratio requires beside")
		}
	}
	if p.Detached && p.Beside != "" {
		return p, bad("placement.detached cannot be used with beside")
	}
	if p.Detached && p.Focus != nil {
		return p, bad("placement.focus cannot be used with detached")
	}
	return p, nil
}

// resolveWorkerPlacement checks the target before the hub admits a spawn intent.
func (h *Hub) resolveWorkerPlacement(ctx context.Context, p workerPlacement) (string, error) {
	sess := p.Session
	if sess == "" {
		sess = h.workSession()
	}
	if sess == "" {
		return "", bad("the work session is unavailable; retry when it exists")
	}
	if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
		return "", err
	}
	if p.Beside != "" {
		blocks, err := h.blocksIn(ctx, sess)
		if err != nil {
			return "", err
		}
		b, ok := blocks[p.Beside]
		if !ok {
			return "", bad("placement.beside is not a block in placement.session")
		}
		if b.Placement == "detached" || b.WindowID == "" {
			return "", bad("placement.beside must be a placed block")
		}
	}
	return sess, nil
}

func (p workerPlacement) request(s *spawnRow, opts map[string]any) (string, map[string]any) {
	block := map[string]any{"flavor": "com.superlogical.terminal.shell", "label": "lazyrex-spawn:" + s.ID, "options": opts}
	if p.Detached {
		block["session_id"] = s.Session
		return "session.new_block", block
	}
	req := map[string]any{"session_id": s.Session, "focus": false, "layout": map[string]any{"block": block}}
	if p.Beside == "" {
		req["window_label"] = s.Title
		return "session.new_window", req
	}
	direction, side, ratio := "horizontal", "after", 1-p.Ratio
	if p.Direction == "above" || p.Direction == "below" {
		direction = "vertical"
	}
	if p.Direction == "left" || p.Direction == "above" {
		side, ratio = "before", p.Ratio
	}
	req["anchor_block_id"], req["direction"], req["side"], req["ratio"] = p.Beside, direction, side, ratio
	return "session.new_split", req
}

// spawnWorker starts a profile in a new tab. Idempotent per (caller, key): a retry returns the
// first request's spawn. The intent commits before Rex creates anything.
func (h *Hub) spawnWorker(ctx context.Context, p perform, args map[string]any) (any, error) {
	caller, err := h.spawnCaller(ctx, p, args)
	if err != nil {
		return nil, err
	}
	key, _ := args["key"].(string)
	profileName, _ := args["profile"].(string)
	title, _ := args["title"].(string)
	values := map[string]string{}
	if raw, ok := args["args"]; ok && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, bad("args must be an object of strings")
		}
		for k, v := range m {
			str, ok := v.(string)
			if !ok || len(str) > 16000 {
				return nil, bad("arg %q must be text of at most 16000 bytes", k)
			}
			values[k] = str
		}
	}
	if key == "" || len(key) > 128 {
		return nil, bad("key is required (at most 128 characters)")
	}
	if strings.HasPrefix(key, windowKeyPrefix) {
		return nil, bad("key must not start with %q (hub.spawn_window uses it)", windowKeyPrefix)
	}
	placement := workerPlacement{Direction: "right", Ratio: 0.5}
	fingerprint := map[string]any{"profile": profileName, "title": title, "args": values}
	cwdArg := ""
	if raw, ok := args["cwd"]; ok {
		cwdArg, _ = raw.(string)
		if !filepath.IsAbs(cwdArg) {
			return nil, bad("%q must be an absolute directory path", cwdArg)
		}
		// The text as given, only when given: a retry finds its request even after the directory
		// is removed, and older requests keep their fingerprint.
		fingerprint["cwd"] = cwdArg
	}
	if raw, ok := args["placement"]; ok {
		placement, err = parseWorkerPlacement(raw)
		if err != nil {
			return nil, err
		}
		fingerprint["placement"] = raw
	}
	if placement.Focus != nil && args["for"] != "user" {
		return nil, bad("placement.focus is only available for user spawns")
	}
	fp, _ := json.Marshal(fingerprint)
	var prevReq string
	row, err := h.callerKeyRow(ctx, "spawns", spawnCols+", request", caller, key)
	if err != nil {
		return nil, err
	}
	if prev, err := scanSpawn(row, &prevReq); err == nil {
		if prevReq != string(fp) {
			return nil, bad("key %q was used for a different request", key)
		}
		if _, err := h.workerBlocks(ctx, prev); err != nil {
			return nil, err
		}
		return prev.result(), nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	byID, _, err := h.loadProfiles()
	if err != nil {
		return nil, err
	}
	prof, ok := byID[profileName]
	if !ok {
		return nil, bad("no profile %q", profileName)
	}
	if len([]rune(title)) > 60 {
		return nil, bad("title is at most 60 characters")
	}
	resolved, err := resolveArgs(prof, values)
	if err != nil {
		return nil, err
	}

	sess, err := h.resolveWorkerPlacement(ctx, placement)
	if err != nil {
		return nil, err
	}
	// Commit the intent: the spawn row and, for an agent profile, its new agent.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	s := &spawnRow{ID: randomID("spawn-"), Caller: caller, Key: key, Profile: prof.ID, Parent: caller, Status: "prepared", Created: now, Session: sess}
	if s.Cwd, err = h.effectiveCwd(prof, cwdArg); err != nil {
		return nil, err
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	name, err := h.admitSpawn(ctx, tx, s, prof, title, string(fp), resolved)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	h.spawnAdmitted(s, prof, name)
	if err := h.markSending(ctx, s); err != nil {
		if u := (*spawnUnrecorded)(nil); errors.As(err, &u) {
			return nil, err
		}
		return s.result(), nil // nothing was sent: the spawn failed (not_created)
	}
	argv := spawnArgv(s, prof, resolved)
	// Rex's default login setting: the worker gets the user's login environment, like a pane the
	// user opens. With login false it gets the Rex server's environment (observed in the lab, 1203).
	opts := map[string]any{"command": argv, "exit": map[string]any{"on_completion": false}}
	opts["cwd"] = s.Cwd // observed on 1203: a shell block honors options.cwd
	method, req := placement.request(s, opts)
	var out struct {
		WindowID string   `json:"window_id"`
		BlockIDs []string `json:"block_ids"`
		BlockID  string   `json:"block_id"`
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := h.spawnCreateFaulted(cctx, s.Key, method, req, &out); err != nil {
		var re *rex.RexError
		if errors.As(err, &re) && re.Code != "disconnected" {
			h.failSpawn(ctx, s, err.Error()) // Rex answered: nothing was created
			return s.result(), nil
		}
		h.kickSpawns()
		return s.result(), nil // outcome unknown: resolveSpawns finds the pane by its label later
	}
	if placement.Detached && out.BlockID != "" {
		out.BlockIDs = []string{out.BlockID}
	}
	if len(out.BlockIDs) != 1 || out.BlockIDs[0] == "" {
		// Rex said yes, so a pane may exist: the outcome is unknown, as for a lost answer.
		log.Printf("spawn %s: unexpected %s result %+v; outcome unknown", s.ID, method, out)
		h.kickSpawns()
		return s.result(), nil
	}
	if placement.Beside != "" {
		out.WindowID = h.windowOf(ctx, s.Session, out.BlockIDs[0])
	}
	if err := h.openSpawn(ctx, s, out.WindowID, out.BlockIDs[0]); err != nil {
		return nil, err
	}
	h.focusSpawn(ctx, s.Session, out.BlockIDs[0], placement.Focus)
	return s.result(), nil
}

// effectiveCwd is always explicit (the argument, the profile's, else the home directory) and
// passed to Rex, so the record says where the worker runs (cwd locks depend on it).
func (h *Hub) effectiveCwd(prof Profile, cwdArg string) (string, error) {
	dir := cwdArg
	if dir == "" {
		dir = prof.Cwd
	}
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	if h.cfg.Test {
		if err := checkPath(dir, h.cfg.TestRoots); err != nil {
			return "", bad("test mode: cwd %s", err)
		}
	}
	return realDir(dir)
}

// admitSpawn inserts one spawn row in tx and, for an agent profile, its new agent. It returns the
// agent's name. The caller commits, then calls spawnAdmitted.
func (h *Hub) admitSpawn(ctx context.Context, tx *sql.Tx, s *spawnRow, prof Profile, title, request string, effective map[string]string) (string, error) {
	if err := keyTakenIn(ctx, tx, "spawns", s.Caller, s.Key); err != nil {
		return "", err
	}
	now := s.Created
	name := ""
	if prof.Kind == "agent" {
		h.mu.Lock()
		taken := func(id string) bool { return h.agents[id] != nil }
		h.mu.Unlock()
		var n int64
		tx.QueryRowContext(ctx, `SELECT value FROM counters WHERE name = ?`, "agent/"+prof.ID).Scan(&n)
		for {
			n++
			s.Agent = fmt.Sprintf("%s-%d", prof.ID, n)
			h.mu.Lock()
			used := taken(s.Agent)
			h.mu.Unlock()
			var exists int
			tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM dyn_agents WHERE agent = ?`, s.Agent).Scan(&exists)
			if !used && exists == 0 {
				break
			}
		}
		name = fmt.Sprintf("%s %d", prof.Name, n)
		if _, err := tx.ExecContext(ctx, `INSERT INTO counters VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET value = excluded.value`, "agent/"+prof.ID, n); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dyn_agents VALUES (?, ?, ?, ?, ?)`, s.Agent, name, prof.ID, s.ID, now); err != nil {
			return "", err
		}
	}
	s.Title = title
	if s.Title == "" {
		s.Title = prof.Name
		if name != "" {
			s.Title = name
		}
	}
	// The lock check, the directory check and the admission share this transaction (the store has
	// one connection, so transactions run one at a time): no spawn enters a locked path, and a
	// removal that locked, removed and unlocked before this point leaves no directory to admit.
	if path, owner, err := lockedPath(ctx, tx, s.Cwd); err != nil {
		return "", err
	} else if path != "" {
		return "", bad("conflict: %s is locked by %s", path, owner)
	}
	if again, err := realDir(s.Cwd); err != nil || again != s.Cwd {
		return "", bad("conflict: %s changed or was removed before admission", s.Cwd)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO spawns (`+spawnCols+`, request) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Caller, s.Key, s.Profile, s.Title, s.Agent, s.Parent, s.Status, "", "", "", "", s.Created, s.Session, s.Cwd, request); err != nil {
		return "", err
	}
	eff, _ := json.Marshal(effective)
	if _, err := tx.ExecContext(ctx, `UPDATE spawns SET effective_args = ?, generation = ? WHERE spawn_id = ?`, string(eff), h.generation, s.ID); err != nil {
		return "", err
	}
	return name, nil
}

// spawnAdmitted runs after the admission commits: the new agent becomes known in memory.
func (h *Hub) spawnAdmitted(s *spawnRow, prof Profile, name string) {
	if s.Agent != "" {
		h.mu.Lock()
		h.agents[s.Agent] = &Agent{ID: s.Agent, Name: name, Profile: prof.ID, delivered: map[string]int64{}}
		h.agentOrder = append(h.agentOrder, s.Agent)
		h.mu.Unlock()
	}
	h.ev.emit("spawn_intent", map[string]any{"spawn_id": s.ID, "profile": prof.ID, "caller": s.Caller, "agent": s.Agent})
}

// spawnArgv is the pane command. Rex has no env option on a spawn (roadmap #1, rechecked on
// 1203): use /usr/bin/env. Every pane drops the agent variables, also an accessory: the user can
// start an agent in it. LAZYREX_AGENT too: only an agent spawn sets it again, so a stale one
// never reaches a shell.
func spawnArgv(s *spawnRow, prof Profile, resolved map[string]string) []string {
	argv := []string{"/usr/bin/env", "-u", "LAZYREX_AGENT"}
	for _, k := range agentEnvUnset {
		argv = append(argv, "-u", k)
	}
	if s.Agent != "" {
		argv = append(argv, "LAZYREX_AGENT="+s.Agent)
	}
	keys := make([]string, 0, len(prof.Env))
	for k := range prof.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, k+"="+prof.Env[k])
	}
	var cmd []string
	for _, a := range prof.Command {
		if m := argToken.FindStringSubmatch(a); m != nil {
			if v := resolved[m[1]]; v != "" {
				cmd = append(cmd, v)
			}
			continue
		}
		cmd = append(cmd, a)
	}
	if !filepath.IsAbs(cmd[0]) {
		if found, err := exec.LookPath(cmd[0]); err == nil {
			cmd[0] = found
		}
	}
	return append(argv, cmd...)
}

// spawnCreate sends the one Rex request that creates a worker's pane. Tests replace it to garble
// the answer.
var spawnCreate = func(ctx context.Context, h *Hub, method string, req map[string]any, out any) error {
	return h.rex.Call(ctx, method, req, out)
}

// openSpawn records the pane. The spawn row and the agent's binding commit together, so a crash
// cannot leave an open agent spawn without its pane.
func (h *Hub) openSpawn(ctx context.Context, s *spawnRow, window, block string) error {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE spawns SET status = 'open', window_id = ?, block_id = ?, session_id = ?, error = '' WHERE spawn_id = ? AND status = 'sending'`, window, block, s.Session, s.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("spawn %s is no longer sending", s.ID)
	}
	if s.Agent != "" {
		// The pane belongs to this agent from now on. Its process joins with LAZYREX_AGENT.
		if _, err := tx.ExecContext(ctx, `INSERT INTO bindings (agent, session_id, block_id, child_pid, child_start) VALUES (?, ?, ?, 0, '')
			ON CONFLICT (agent) DO UPDATE SET session_id = excluded.session_id, block_id = excluded.block_id`, s.Agent, s.Session, block); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.Agent != "" {
		h.mu.Lock()
		if a := h.agents[s.Agent]; a != nil {
			a.Session, a.BlockID = s.Session, block
			h.syncPrompts()
		}
		h.mu.Unlock()
	}
	s.Status, s.Window, s.Block, s.Error = "open", window, block, ""
	h.ev.emit("spawn_opened", map[string]any{"spawn_id": s.ID, "block_id": block, "agent": s.Agent})
	h.changed("workers", "agents")
	return nil
}

// spawnNotCreated starts the error of a spawn whose create request was never sent. Nothing of it
// exists in Rex, so a retry with a new key is safe.
const spawnNotCreated = "not_created"

// markSending records, in one transaction, that the create request of rows may be sent now. It runs
// after the admission commits and before the request, so a hub that stops in between leaves the
// rows prepared, and the next hub fails them as not created (failUnsentSpawns). Each row must still
// be prepared. On an error nothing is sent, and the rows fail as not created.
func (h *Hub) markSending(ctx context.Context, rows ...*spawnRow) error {
	err := h.setSending(ctx, rows)
	if err == nil {
		for _, s := range rows {
			s.Status = "sending"
		}
		return nil
	}
	// A request context that ended must not leave the rows prepared until the next hub.
	fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	why := spawnNotCreated + ": " + err.Error()
	var lost error
	for _, s := range rows {
		if ferr := h.failSpawn(fctx, s, why); ferr != nil && !errors.Is(ferr, errSpawnMoved) {
			// The row stays prepared and holds its quota and cwd lock: resolveNext stores the
			// failure when the database takes it.
			h.mu.Lock()
			if h.unsentSpawns == nil {
				h.unsentSpawns = map[string]string{}
			}
			h.unsentSpawns[s.ID] = why
			h.mu.Unlock()
			lost = ferr
		}
	}
	if lost != nil {
		h.kickSpawns()
		return &spawnUnrecorded{err: err, lost: lost}
	}
	return err
}

// spawnUnrecorded is the error of markSending when the hub sent nothing and could not store the
// failure either. The caller returns it: a failed result would not be durable, and a replay of the
// key reads the stored row (pending until resolveNext stores the failure).
type spawnUnrecorded struct{ err, lost error }

func (e *spawnUnrecorded) Error() string {
	return fmt.Sprintf("%s: %v; the hub could not store the failure (%v) and stores it later", spawnNotCreated, e.err, e.lost)
}

// retryUnsent stores the failures that markSending could not store.
func (h *Hub) retryUnsent(ctx context.Context) error {
	h.mu.Lock()
	unsent := make(map[string]string, len(h.unsentSpawns))
	for id, why := range h.unsentSpawns {
		unsent[id] = why
	}
	h.mu.Unlock()
	for id, why := range unsent {
		if err := h.failSpawn(ctx, &spawnRow{ID: id}, why); err != nil && !errors.Is(err, errSpawnMoved) {
			return err
		}
		h.mu.Lock()
		delete(h.unsentSpawns, id)
		h.mu.Unlock()
	}
	return nil
}

func (h *Hub) setSending(ctx context.Context, rows []*spawnRow) error {
	if err := h.spawnSendingFault(rows); err != nil {
		return err
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, s := range rows {
		res, err := tx.ExecContext(ctx, `UPDATE spawns SET status = 'sending' WHERE spawn_id = ? AND status = 'prepared'`, s.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("spawn %s is no longer prepared", s.ID)
		}
	}
	return tx.Commit()
}

// failUnsentSpawns fails each prepared spawn of an older hub generation. That hub stopped after the
// admission and before it marked the row sending, so it sent no create request, and no pane of the
// spawn exists. A sending row stays: its request may have reached Rex, and one empty search does
// not prove that no pane comes (rex-protocol.md, "A request outlives its client").
func (h *Hub) failUnsentSpawns(ctx context.Context) error {
	rows, err := h.db.QueryContext(ctx, `SELECT spawn_id FROM spawns WHERE status = 'prepared' AND generation < ?`, h.generation)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	why := spawnNotCreated + ": the hub stopped before it sent the create request"
	for _, id := range ids {
		res, err := h.db.ExecContext(ctx, `UPDATE spawns SET status = 'failed', error = ? WHERE spawn_id = ? AND status = 'prepared'`, why, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			h.ev.emit("spawn_failed", map[string]any{"spawn_id": id, "error": why})
		}
	}
	return nil
}

// errSpawnMoved: failSpawn found the row no longer prepared or sending, so it changed nothing.
var errSpawnMoved = errors.New("the spawn is no longer pending")

// failSpawn stores a failure. It changes s, emits the event and reports the change only after the
// update is stored.
func (h *Hub) failSpawn(ctx context.Context, s *spawnRow, why string) error {
	// The agent ID stays retired: join refuses an agent whose spawn failed.
	res, err := h.db.ExecContext(ctx, `UPDATE spawns SET status = 'failed', error = ? WHERE spawn_id = ? AND status IN ('prepared', 'sending')`, why, s.ID)
	if err != nil {
		log.Printf("spawn %s: store the failure: %v", s.ID, err)
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errSpawnMoved
	}
	s.Status, s.Error = "failed", why
	h.ev.emit("spawn_failed", map[string]any{"spawn_id": s.ID, "error": why})
	h.changed("workers")
	return nil
}

// resolveSpawns reports spawns whose Rex request had no known outcome, for example after a hub
// crash. A label is only a discovery hint (architecture.md), so this never adopts or closes a pane
// by its label. It records how many candidate panes exist, for the worker list. An agent spawn
// opens when its process joins from the one candidate pane (adoptByJoin). An accessory spawn
// stays pending, visible in the worker list, and hub.stop refuses it (known limit of v1).
// It runs at start and on Rex events that can change the answer (kickSpawns): a block created or
// closed, a session destroyed, an agent join. A timer is armed only for a pending spawn that is
// still in its first-request window, and after an error.
func (h *Hub) resolveSpawns(ctx context.Context) {
	for {
		next, err := h.resolveNext(ctx)
		if err != nil {
			log.Printf("spawns: %v", err)
			next = time.Now().Add(10 * time.Second)
		}
		var due <-chan time.Time
		if !next.IsZero() {
			due = time.After(time.Until(next))
		}
		select {
		case <-ctx.Done():
			return
		case <-h.spawnKick:
		case <-due:
		}
	}
}

// kickSpawns wakes resolveSpawns. Repeated kicks coalesce.
func (h *Hub) kickSpawns() {
	if h.spawnKick == nil {
		return
	}
	select {
	case h.spawnKick <- struct{}{}:
	default:
	}
}

func (h *Hub) resolveOnce(ctx context.Context) error {
	_, err := h.resolveNext(ctx)
	return err
}

// resolveNext runs one pass. It returns when a pending spawn leaves its first-request window
// (zero: no such spawn), so the caller can wake for it without polling.
func (h *Hub) resolveNext(ctx context.Context) (next time.Time, err error) {
	if err := h.retryUnsent(ctx); err != nil {
		return time.Time{}, err
	}
	rows, err := h.db.QueryContext(ctx, `SELECT `+spawnCols+` FROM spawns WHERE status IN ('sending', 'open')`)
	if err != nil {
		return time.Time{}, err
	}
	var pending []*spawnRow
	for rows.Next() {
		if s, err := scanSpawn(rows); err == nil {
			pending = append(pending, s)
		}
	}
	rows.Close()
	if len(pending) == 0 {
		return time.Time{}, nil
	}
	for _, s := range pending {
		blocks, err := h.workerBlocks(ctx, s)
		if err != nil {
			return time.Time{}, err
		}
		if s.Status != "sending" || s.Session == "" {
			continue
		}
		if ready := createdAt(s.Created).Add(15 * time.Second); time.Now().Before(ready) {
			if next.IsZero() || ready.Before(next) {
				next = ready
			}
			continue // the first request may still be running in spawnWorker
		}
		n := len(spawnCandidates(blocks, s.ID))
		why := "outcome unknown: no pane found"
		if n > 0 {
			why = fmt.Sprintf("outcome unknown: %d candidate pane(s)", n)
		}
		if s.Agent != "" && n == 1 {
			why += "; waiting for the agent to join"
		}
		if why != s.Error {
			h.db.ExecContext(ctx, `UPDATE spawns SET error = ? WHERE spawn_id = ? AND status = 'sending'`, why, s.ID)
			h.ev.emit("spawn_unresolved", map[string]any{"spawn_id": s.ID, "candidates": n})
			h.changed("workers")
		}
	}
	return next, nil
}

// workerBlocks keeps recovery on the session recorded before the Rex request.
// An empty legacy session is unknown. Only an exact recorded block can recover it.
func (h *Hub) workerBlocks(ctx context.Context, s *spawnRow) (map[string]rexBlock, error) {
	if s.Status != "sending" && s.Status != "open" {
		return nil, nil
	}
	if s.Session == "" {
		if s.Block != "" {
			var list struct {
				Sessions []struct {
					ID string `json:"session_id"`
				} `json:"sessions"`
			}
			if err := h.rex.Call(ctx, "session.list", map[string]any{}, &list); err != nil {
				return nil, err
			}
			for _, sess := range list.Sessions {
				blocks, err := h.workerSessionBlocks(ctx, sess.ID)
				if sessionGone(err) {
					continue
				}
				if err != nil {
					return nil, err
				}
				if _, ok := blocks[s.Block]; ok {
					if _, err := h.db.ExecContext(ctx, `UPDATE spawns SET session_id = ? WHERE spawn_id = ? AND session_id = ''`, sess.ID, s.ID); err != nil {
						return nil, err
					}
					s.Session = sess.ID
					break
				}
			}
		}
		if s.Session == "" {
			why := "outcome unknown: legacy worker session is not recorded"
			if _, err := h.db.ExecContext(ctx, `UPDATE spawns SET error = ? WHERE spawn_id = ? AND status IN ('sending', 'open')`, why, s.ID); err != nil {
				return nil, err
			}
			s.Error = why
			return nil, nil
		}
	}
	blocks, err := h.workerSessionBlocks(ctx, s.Session)
	if sessionGone(err) {
		// The session cannot accept a late request. Never start a replacement worker.
		return nil, h.workerGone(ctx, s, "session gone")
	}
	if err != nil {
		return nil, err
	}
	if s.Status == "open" {
		if _, ok := blocks[s.Block]; !ok {
			return nil, h.workerGone(ctx, s, "pane gone")
		}
		why := ""
		if why != s.Error {
			if _, err := h.db.ExecContext(ctx, `UPDATE spawns SET error = ? WHERE spawn_id = ? AND status = 'open'`, why, s.ID); err != nil {
				return nil, err
			}
			s.Error = why
		}
	}
	return blocks, nil
}

// A previous session may not be attached after a hub restart.
func (h *Hub) workerSessionBlocks(ctx context.Context, sess string) (map[string]rexBlock, error) {
	if sess != h.workSession() {
		if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
			return nil, err
		}
	}
	return h.blocksIn(ctx, sess)
}

// workerGone records confirmed loss and reloads the row if another handler won the update.
func (h *Hub) workerGone(ctx context.Context, s *spawnRow, why string) error {
	status := "closed"
	event := "spawn_closed"
	if s.Status == "sending" {
		status, event = "failed", "spawn_failed"
	}
	res, err := h.db.ExecContext(ctx, `UPDATE spawns SET status = ?, error = ?, closed_by = ? WHERE spawn_id = ? AND status = ?`, status, why, why, s.ID, s.Status)
	if err != nil {
		return err
	}
	current, err := h.spawnByID(ctx, s.ID)
	if err != nil {
		return err
	}
	*s = *current
	if n, _ := res.RowsAffected(); n == 1 {
		h.ev.emit(event, map[string]any{"spawn_id": s.ID, "session_id": s.Session, "error": why})
		h.changed("workers", "agents")
	}
	return nil
}

func spawnCandidates(blocks map[string]rexBlock, spawnID string) []string {
	var out []string
	for id, b := range blocks {
		if b.Label == "lazyrex-spawn:"+spawnID {
			out = append(out, id)
		}
	}
	return out
}

// adoptByJoin opens a pending agent spawn when its process joins. The caller's pane, found from
// the caller's PID, is the proof; the label only rules out other panes. It returns the spawn's
// status after the attempt.
func (h *Hub) adoptByJoin(ctx context.Context, s *spawnRow, block string) (string, error) {
	blocks, err := h.workerBlocks(ctx, s)
	if err != nil {
		return "", err
	}
	if s.Status != "sending" {
		return s.Status, nil
	}
	if s.Session == "" {
		// join already proved that this caller runs in block. Recover its session by exact ID.
		s.Session, err = h.findBlockSession(ctx, block)
		if err != nil {
			return "", err
		}
		if s.Session == "" {
			return "sending", nil
		}
		blocks, err = h.workerSessionBlocks(ctx, s.Session)
		if err != nil {
			return "", err
		}
	}
	c := spawnCandidates(blocks, s.ID)
	if len(c) != 1 || c[0] != block {
		return "sending", nil
	}
	if err := h.openSpawn(ctx, s, h.windowOf(ctx, s.Session, block), block); err != nil {
		return "", err
	}
	h.ev.emit("spawn_adopted", map[string]any{"spawn_id": s.ID, "block_id": block})
	return "open", nil
}

func (h *Hub) windowOf(ctx context.Context, sess, block string) string {
	var sv struct {
		Windows []struct {
			WindowID string `json:"window_id"`
			Layers   []struct {
				Blocks []struct {
					BlockID string `json:"block_id"`
				} `json:"blocks"`
			} `json:"layers"`
		} `json:"windows"`
	}
	if h.rex.Call(ctx, "session.view", map[string]any{"session_id": sess}, &sv) != nil {
		return ""
	}
	for _, w := range sv.Windows {
		for _, l := range w.Layers {
			for _, b := range l.Blocks {
				if b.BlockID == block {
					return w.WindowID
				}
			}
		}
	}
	return ""
}

// workersList reports every spawn that is not closed, with its state: starting, running, exited
// or failed. A failed spawn stays in the list, with its error, until hub.stop dismisses it.
func (h *Hub) workersList(ctx context.Context) (any, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT `+spawnCols+` FROM spawns WHERE status IN ('prepared', 'sending', 'open', 'failed') ORDER BY created`)
	if err != nil {
		return nil, err
	}
	var list []*spawnRow
	for rows.Next() {
		if s, err := scanSpawn(rows); err == nil {
			list = append(list, s)
		}
	}
	rows.Close()
	out := []any{}
	for _, s := range list {
		blocks, err := h.workerBlocks(ctx, s)
		if err != nil {
			return nil, err
		}
		if s.Status == "closed" {
			continue
		}
		state := "starting"
		var status any
		if s.Status == "failed" {
			state = "failed"
		}
		if s.Status == "open" && blocks != nil {
			if pid, err := h.childPIDIn(ctx, s.Session, s.Block); err == nil && pid > 0 {
				state = "running"
			} else {
				state = "exited"
			}
			status = h.rootStatus(ctx, s.Session, s.Block)
		}
		w := map[string]any{"spawn_id": s.ID, "profile": s.Profile, "title": s.Title, "parent": s.Parent,
			"state": state, "status": status, "session_id": s.Session, "cwd": s.Cwd}
		if s.Agent != "" {
			w["agent_id"] = s.Agent
		}
		if s.Block != "" {
			w["block_id"], w["window_id"] = s.Block, s.Window
		}
		if s.Error != "" {
			w["error"] = s.Error
		}
		out = append(out, w)
	}
	return map[string]any{"workers": out}, nil
}

// rootStatus returns the block's OSC 7501 root record as {state, msg}, or nil.
func (h *Hub) rootStatus(ctx context.Context, sess, block string) any {
	var st struct {
		Records []struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Msg   string `json:"msg"`
		} `json:"records"`
	}
	if h.rex.BlockCall(ctx, "com.superlogical.terminal.program_status", sess, block, nil, &st) != nil {
		return nil
	}
	for _, r := range st.Records {
		if r.ID == "" {
			return map[string]any{"state": r.State, "msg": r.Msg}
		}
	}
	return nil
}

func (h *Hub) spawnByID(ctx context.Context, id string) (*spawnRow, error) {
	s, err := scanSpawn(h.db.QueryRowContext(ctx, `SELECT `+spawnCols+` FROM spawns WHERE spawn_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, bad("no spawn %q", id)
	}
	return s, err
}

// stop closes a worker's pane. The user and every agent may stop any worker, also one that another
// identity started (decision "Agents may act on anything", 2026-10-19: agents may act on
// anything a user can).
// Stop is idempotent by spawn_id: a retry of a finished stop returns status closed. The optional
// key is accepted so that the renderer can send its operation key, but stop does not need it.
// Stop refuses a pending spawn: its pane is not identified, and its process can still start.
// A failed spawn is dismissed from the worker list.
func (h *Hub) stop(ctx context.Context, p perform, args map[string]any) (any, error) {
	caller, err := h.spawnCaller(ctx, p, args)
	if err != nil {
		return nil, err
	}
	id, _ := args["spawn_id"].(string)
	s, err := h.spawnByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := h.workerBlocks(ctx, s); err != nil {
		return nil, err
	}
	switch s.Status {
	case "closed":
		return map[string]any{"spawn_id": id, "status": "closed"}, nil
	case "prepared", "sending":
		return nil, bad("not_ready: %s has no identified pane yet, so the hub cannot stop it. An agent spawn opens when its agent joins; otherwise close its tab by hand", id)
	case "failed":
		res, err := h.db.ExecContext(ctx, `UPDATE spawns SET status = 'closed', closed_by = ? WHERE spawn_id = ? AND status = 'failed'`, caller, id)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			h.ev.emit("spawn_closed", map[string]any{"spawn_id": id, "by": caller, "was": s.Status})
			h.changed("workers", "agents")
		}
		return map[string]any{"spawn_id": id, "status": "closed"}, nil
	}
	if s.Session == "" {
		return nil, bad("not_ready: the session of %s is unknown", id)
	}
	h.mu.Lock()
	h.selfClosed[s.Block] = true
	h.mu.Unlock()
	if err := h.rex.Call(ctx, "block.close", map[string]any{"session_id": s.Session, "block_id": s.Block}, nil); err != nil {
		var re *rex.RexError
		if !errors.As(err, &re) || re.Code != "not_found" {
			return nil, err
		}
	}
	h.closeSpawn(ctx, s, caller)
	return map[string]any{"spawn_id": id, "status": "closed"}, nil
}

func (h *Hub) closeSpawn(ctx context.Context, s *spawnRow, by string) {
	res, err := h.db.ExecContext(ctx, `UPDATE spawns SET status = 'closed', closed_by = ? WHERE spawn_id = ? AND status = 'open'`, by, s.ID)
	if err != nil {
		log.Printf("spawns: close %s: %v", s.ID, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 1 {
		s.Status, s.ClosedBy = "closed", by
		h.ev.emit("spawn_closed", map[string]any{"spawn_id": s.ID, "by": by})
		h.changed("workers", "agents")
	}
}

// workerBlockClosed records a worker pane that closed (by the user or by stop).
func (h *Hub) workerBlockClosed(ctx context.Context, blockID string) bool {
	s, err := scanSpawn(h.db.QueryRowContext(ctx, `SELECT `+spawnCols+` FROM spawns WHERE block_id = ? AND status = 'open'`, blockID))
	if err != nil {
		return false
	}
	h.closeSpawn(ctx, s, "pane closed")
	return true
}

func (h *Hub) navigateWorker(ctx context.Context, args map[string]any) (any, error) {
	id, _ := args["spawn_id"].(string)
	s, err := h.spawnByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.Status != "open" {
		return nil, bad("%s is %s", id, s.Status)
	}
	// The window comes from Rex, not the spawn row: an app-mode restore rebuilds the tab with a new
	// window ID (Codex's review).
	blocks, err := h.workerBlocks(ctx, s)
	if err != nil {
		return nil, err
	}
	b, ok := blocks[s.Block]
	if !ok {
		return nil, bad("the tab of %s is gone", id)
	}
	if b.Placement == "detached" {
		return nil, bad("this worker is detached and has no visible tab")
	}
	go h.showOnApps(context.Background(), "", s.Session, &view{ID: s.ID, Tab: "worker/" + s.ID, Window: b.WindowID, Block: s.Block})
	return map[string]any{"spawn_id": id, "status": "navigating"}, nil
}

// navigateAgent handles hub.navigate.agent {agent}: it shows the tab of a live agent (an identity
// or a legacy agent) on the apps, like navigateWorker. Plugins call it for a click on an agent.
func (h *Hub) navigateAgent(ctx context.Context, args map[string]any) (any, error) {
	id, _ := args["agent"].(string)
	h.mu.Lock()
	a := h.idents[id]
	if a == nil || a.IdState != "current" {
		a = h.agents[id]
	}
	var sess, block string
	if a != nil && a.live() {
		sess, block = h.bindingSession(a), a.BlockID
	}
	h.mu.Unlock()
	if block == "" {
		return nil, bad("%s has no open tab", id)
	}
	blocks, err := h.workerSessionBlocks(ctx, sess)
	if err != nil {
		return nil, err
	}
	b, ok := blocks[block]
	if !ok {
		return nil, bad("the tab of %s is gone", id)
	}
	if b.Placement == "detached" {
		return nil, bad("%s is detached and has no visible tab", id)
	}
	go h.showOnApps(context.Background(), "", sess, &view{ID: id, Window: b.WindowID, Block: block})
	return map[string]any{"agent": id, "status": "navigating"}, nil
}

// candidateChange gives the "candidate" object of a passing entry with a run (section 7): "commit"
// when the candidate commit differs from the run commit, and "dir" when the candidate dir differs
// from the run dir. A local candidate of a GitHub run has the commit null. It gives nil when there
// is no candidate or neither field differs.
func candidateChange(cand, run *pluginRun) map[string]any {
	if cand == nil {
		return nil
	}
	c := map[string]any{}
	if cand.Commit != run.Commit {
		c["commit"] = cand.Commit
		if cand.Commit == "" {
			c["commit"] = nil // a local candidate: no commit
		}
	}
	if cand.Dir != run.Dir {
		c["dir"] = cand.Dir
	}
	if len(c) == 0 {
		return nil
	}
	return c
}

// pluginsList reports (docs/plugin-sources.md, section 7):
//   - plugins: each registered plugin and its declared tabs, for navigation;
//   - refused: each config entry in state refused, with its error;
//   - entries: each config entry with its state (running, stopped, refused or retained), the dir
//     and the commit of its run, its error when refused or retained, and "candidate": {"error"}
//     when its last resolution refused it, or the candidate fields that differ from the run
//     (candidateChange; for example hub.plugins.reload changed the source, or an update the commit).
//   - kept: each record without an entry (plugins.go loadKept), with its name, source, state, error
//     and the dir and the commit of its run.
//
// An entry that no manifest named has name null.
func (h *Hub) pluginsList(ctx context.Context) (any, error) {
	h.entriesMu.Lock()
	h.mu.Lock()
	var running []pluginRun
	entries, refused := []any{}, []any{}
	for _, e := range h.entries {
		var name any
		if e.seen != "" {
			name = e.seen // a refused entry that a manifest named
		}
		state, errText := "refused", ""
		if e.err != nil {
			errText = e.err.Error()
		}
		item := e.ref()
		if pl := h.plugins[e.name]; e.name != "" && pl != nil {
			name, state = e.name, pl.reportedState()
			if pl.state != "" {
				errText = pl.refusal
			}
			if pl.state == "waiting" {
				item["waiting"] = pl.waiting
			}
			if pl.run.Name != "" {
				item["dir"] = pl.run.Dir
				if pl.run.Commit != "" {
					item["commit"] = pl.run.Commit
				}
				if c := candidateChange(e.cand, &pl.run); c != nil {
					item["candidate"] = c
				}
			}
			if pl.ClientID != "" {
				running = append(running, pl.run)
			}
			// The owned instance (hub.admin.replace takes it as expect_instance): "" when none.
			id, pending := h.ownedInstance(pl)
			item["instance_id"], item["instance_pending"] = id, pending
			item["desired"], item["registered"] = pl.Desired, h.registeredLocked(e.name)
		}
		item["name"], item["state"] = name, state
		if state == "refused" || state == "retained" {
			item["error"] = errText
		}
		if e.err != nil {
			item["candidate"] = map[string]any{"error": e.err.Error()}
		}
		if state == "refused" {
			r := e.ref()
			r["name"], r["state"], r["error"] = name, state, errText
			refused = append(refused, r)
		}
		entries = append(entries, item)
	}
	if h.managedErr != nil {
		// The file once, not each entry: a malformed file gives no entries (section 13).
		refused = append(refused, map[string]any{"managed_file": h.cfg.managedPath, "error": h.managedErr.Error()})
	}
	kept := []any{}
	for _, pl := range h.plugins {
		if pl.entry == nil {
			k := map[string]any{"name": pl.Name, "source": pl.source, "state": pl.reportedState(), "error": pl.refusal, "dir": pl.run.Dir}
			if pl.run.Commit != "" {
				k["commit"] = pl.run.Commit
			}
			kept = append(kept, k)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].(map[string]any)["name"].(string) < kept[j].(map[string]any)["name"].(string) })
	h.mu.Unlock()
	h.entriesMu.Unlock()
	out := []any{}
	for _, c := range running {
		name := c.Name
		tabs, err := h.loadTabs(ctx, name)
		if err != nil {
			return nil, err
		}
		list := []any{}
		for _, t := range tabs {
			list = append(list, map[string]any{"key": t.Key, "title": t.Title, "route": t.Route})
		}
		entry := map[string]any{"plugin": name, "id": name, "name": name, "tabs": list, "source": c.Source, "dir": c.Dir}
		if cmd := h.landingPanel(ctx, name); cmd != "" {
			entry["panel"] = cmd // the panel command a Home click opens (panels.go)
		}
		if len(tabs) > 0 {
			entry["tab"] = tabs[0].Key
			entry["name"] = stripCount(tabs[0].Title)
		}
		out = append(out, entry)
	}
	return map[string]any{"plugins": out, "refused": refused, "entries": entries, "kept": kept}, nil
}

var countSuffix = regexp.MustCompile(` \(\d+\)$`)

func stripCount(title string) string { return countSuffix.ReplaceAllString(title, "") }

// changed tells every registered plugin that hub data changed, and renders its views again.
// It never runs inside a hub handler's call to a plugin: each perform has its own goroutine.
func (h *Hub) changed(what ...string) {
	for _, w := range what {
		if w == "agents" || w == "plugins" { // readiness or a plugin's registration changed
			h.kickNotices()
		}
		if w == "plugins" {
			// A registration that ended stops its prompt sender. Non-blocking: it takes h.mu (so no
			// caller of changed holds h.mu; changed takes it below too), closes channels and deletes
			// map entries. No Rex call and no store call.
			h.reapPromptSenders()
		}
	}
	// The client IDs are read under h.mu: a registration can change them after the unlock.
	type target struct{ client, name string }
	h.mu.Lock()
	var targets []target
	for _, p := range h.plugins {
		if p.ClientID != "" {
			targets = append(targets, target{p.ClientID, p.Name})
		}
	}
	h.mu.Unlock()
	for _, p := range targets {
		for _, w := range what {
			h.deliverChanged(p.client, p.name, w) // coalesced (changed.go)
		}
		h.kickViews(p.name, true)
	}
}

// checkProfiles reports a changed profile file to the plugins. Rex has no file events, so the hub
// checks (one stat per file) when someone may look: a view gets focus, or a caller lists profiles.
// Spawns always read the files, so they never use a stale profile.
func (h *Hub) checkProfiles() {
	var b strings.Builder
	for _, f := range h.profileFiles() {
		if st, err := os.Stat(f); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", f, st.ModTime().UnixNano(), st.Size())
		}
	}
	h.mu.Lock()
	now, changed := b.String(), h.profileStamp != b.String()
	first := h.profileStamp == "" && !h.profileChecked
	h.profileStamp, h.profileChecked = now, true
	h.mu.Unlock()
	if changed && !first {
		h.ev.emit("profiles_changed", nil)
		go h.changed("profiles")
	}
}

// hubToolActions are hub actions that agents may call through the MCP shim (hub.tools).
var hubToolActions = []map[string]any{
	{"name": "hub.profiles", "kind": "client", "title": "List profiles",
		"description": "List the named commands (profiles) that can start in a new tab, with their kind (agent or accessory), their default cwd (empty: the home directory) and how many run now. args lists the named values that hub.spawn accepts for the profile (name, label, default, required).",
		"args":        map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}},
	{"name": "hub.spawn", "kind": "client", "title": "Start a profile",
		"description": "Start a profile in a new tab, beside a block, or detached, in the chosen session. An agent profile gets its own agent ID, so it can message you. Choose a key; after an unknown outcome, retry with the same key. Returns spawn_id, session_id, status (open, pending, failed or closed) and agent_id.",
		"args": map[string]any{"type": "object", "properties": map[string]any{
			"profile": map[string]any{"type": "string", "minLength": 1}, "key": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"title": map[string]any{"type": "string", "maxLength": 60},
			"cwd":   map[string]any{"type": "string", "minLength": 1, "description": "An existing absolute directory to start in, for example a worktree from worktree_create. Replaces the profile's cwd."},
			"placement": map[string]any{"type": "object", "additionalProperties": false, "description": "Default: a new tab in the work session. Choose another session, split beside a placed block, or start detached.", "properties": map[string]any{
				"session":   map[string]any{"type": "string", "minLength": 1, "description": "Target session ID."},
				"beside":    map[string]any{"type": "string", "minLength": 1, "description": "A placed block ID in the target session."},
				"direction": map[string]any{"type": "string", "enum": []string{"right", "left", "above", "below"}, "description": "Requires beside. Default: right."},
				"ratio":     map[string]any{"type": "number", "exclusiveMinimum": 0, "exclusiveMaximum": 1, "description": "Requires beside. Fraction for the new pane. Default: 0.5."},
				"detached":  map[string]any{"type": "boolean", "description": "Start without a tab. Cannot be combined with beside."},
				"focus":     spawnFocusSchema(),
			}},
			"args": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string", "maxLength": 16000},
				"description": "Named values for the profile, for example {\"task\": \"Fix the login test\"}. hub.profiles lists each profile's args."},
			"for": map[string]any{"type": "string"}, "on_behalf": map[string]any{"type": "integer"}},
			"required": []string{"profile", "key"}, "additionalProperties": false}},
	{"name": "hub.workers", "kind": "client", "title": "List workers",
		"description": "List started profiles that are not closed: spawn_id, session_id, cwd, profile, title, parent, agent_id, state (starting, running, exited, failed), error and program status.",
		"args":        map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}},
	{"name": "hub.stop", "kind": "client", "title": "Stop a worker",
		"description": "Close a worker's tab, or dismiss a failed start. A pending start with an unknown outcome cannot be stopped. Any agent can stop any worker, also one that another agent or the user started. Safe to retry: stop is idempotent by spawn_id, and key is optional.",
		"args": map[string]any{"type": "object", "properties": map[string]any{
			"spawn_id": map[string]any{"type": "string", "minLength": 1}, "key": map[string]any{"type": "string", "maxLength": 128},
			"for": map[string]any{"type": "string"}, "on_behalf": map[string]any{"type": "integer"}},
			"required": []string{"spawn_id"}, "additionalProperties": false}},
}

func createdAt(v string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}

func (h *Hub) isWorkerBlock(ctx context.Context, blockID string) bool {
	var n int
	h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM spawns WHERE block_id = ? AND status = 'open'`, blockID).Scan(&n)
	return n > 0
}

func profileArgs(p Profile) []any {
	out := []any{}
	for _, a := range p.Args {
		label := a.Label
		if label == "" {
			label = a.Name
		}
		out = append(out, map[string]any{"name": a.Name, "label": label, "default": a.Default, "required": a.Required})
	}
	return out
}

// resolveArgs checks the spawn's values against the profile and applies defaults.
func resolveArgs(p Profile, values map[string]string) (map[string]string, error) {
	out := map[string]string{}
	declared := map[string]bool{}
	for _, a := range p.Args {
		declared[a.Name] = true
		v, ok := values[a.Name]
		if !ok || v == "" {
			v = a.Default
		}
		if v == "" && a.Required {
			return nil, bad("arg %q is required for profile %q", a.Name, p.ID)
		}
		out[a.Name] = v
	}
	for k := range values {
		if !declared[k] {
			return nil, bad("profile %q has no arg %q", p.ID, k)
		}
	}
	return out, nil
}

// under reports whether dir is path or inside it.
func under(dir, path string) bool {
	return dir == path || strings.HasPrefix(dir, strings.TrimSuffix(path, "/")+"/")
}

type rowsQuerier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

// realDir returns the absolute directory with symlinks resolved, so a link into a locked tree
// cannot bypass the lock. The directory must exist.
func realDir(text string) (string, error) {
	if !filepath.IsAbs(text) {
		return "", bad("%q must be an absolute directory path", text)
	}
	dir, err := filepath.EvalSymlinks(filepath.Clean(text))
	if err != nil {
		return "", bad("%s is not an existing directory", text)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", bad("%s is not an existing directory", text)
	}
	return dir, nil
}

// lockedPath returns the lock that covers dir, if any.
func lockedPath(ctx context.Context, q rowsQuerier, dir string) (path, owner string, err error) {
	if dir == "" {
		return "", "", nil
	}
	rows, err := q.QueryContext(ctx, `SELECT path, owner FROM cwd_locks`)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&path, &owner); err != nil {
			return "", "", err
		}
		if under(dir, path) {
			return path, owner, nil
		}
	}
	return "", "", rows.Err()
}

func lockPathArg(args map[string]any) (path, key string, err error) {
	text, _ := args["path"].(string)
	if path, err = realDir(text); err != nil {
		return "", "", err
	}
	if key, _ = args["key"].(string); key == "" {
		return "", "", bad("key is required")
	}
	return path, key, nil
}

// liveCwds asks Rex where every terminal pane is now: the child's and the foreground process's
// cwd (from the OS, not self-reported). It returns the panes with a cwd in path. It is a snapshot:
// a shell can still cd into the path after the lock; the lock only refuses new spawns.
func (h *Hub) liveCwds(ctx context.Context, path string) ([]any, error) {
	ids, err := h.sessionIDs(ctx)
	if err != nil {
		return nil, err
	}
	var found []any
	for _, sess := range ids {
		if sess != h.workSession() {
			if err := h.rex.Call(ctx, "session.attach", map[string]any{"session_id": sess, "event_scope": "session"}, nil); err != nil {
				if strings.Contains(err.Error(), "not found") {
					continue // destroyed since the list
				}
				return nil, err
			}
		}
		var blocks struct {
			Blocks []struct {
				BlockID     string `json:"block_id"`
				CreatorName string `json:"creator_name"`
				Label       string `json:"label"`
			} `json:"blocks"`
		}
		if err := h.rex.Call(ctx, "session.list_blocks", map[string]any{"session_id": sess}, &blocks); err != nil {
			if strings.Contains(err.Error(), "not found") {
				continue
			}
			return nil, err
		}
		for _, b := range blocks.Blocks {
			if b.CreatorName != "com.superlogical.terminal" {
				continue
			}
			type proc struct {
				PID int    `json:"pid"`
				Cwd string `json:"cwd"`
			}
			var out struct {
				Child      *proc `json:"child"`
				Foreground *proc `json:"foreground"`
			}
			if err := h.rex.BlockCall(ctx, "com.superlogical.terminal.process", sess, b.BlockID, nil, &out); err != nil {
				if strings.Contains(err.Error(), "not found") {
					continue // closed since the list
				}
				return nil, err
			}
			for _, p := range []*proc{out.Child, out.Foreground} {
				if p == nil || p.Cwd == "" {
					continue
				}
				cwd := p.Cwd
				if real, err := filepath.EvalSymlinks(cwd); err == nil {
					cwd = real
				}
				if under(cwd, path) {
					found = append(found, map[string]any{"session_id": sess, "block_id": b.BlockID, "label": b.Label, "pid": p.PID, "cwd": cwd})
					break
				}
			}
		}
	}
	return found, nil
}

// cwdWorkers lists workers that block removal. Use the same rule for advice and locked admission.
func cwdWorkers(ctx context.Context, q rowsQuerier, path string) ([]any, error) {
	rows, err := q.QueryContext(ctx, `SELECT spawn_id, cwd, status FROM spawns WHERE status IN ('prepared', 'sending', 'open')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	workers := []any{}
	for rows.Next() {
		var id, cwd, status string
		if err := rows.Scan(&id, &cwd, &status); err != nil {
			return nil, err
		}
		if cwd == "" || under(cwd, path) {
			// An unknown cwd may be inside the path: refuse (conservative).
			workers = append(workers, map[string]any{"spawn_id": id, "cwd": cwd, "status": status})
		}
	}
	return workers, rows.Err()
}

// cwdCheck is advice only. It reserves nothing; removal must still acquire a cwd lock.
func (h *Hub) cwdCheck(ctx context.Context, args map[string]any) (any, error) {
	text, _ := args["path"].(string)
	path, err := realDir(text)
	if err != nil {
		return nil, err
	}
	panes, err := h.liveCwds(ctx, path)
	if err != nil {
		return nil, err
	}
	workers, err := cwdWorkers(ctx, h.db, path)
	if err != nil {
		return nil, err
	}
	if panes == nil {
		panes = []any{}
	}
	return map[string]any{"path": path, "available": len(workers) == 0 && len(panes) == 0, "workers": workers, "panes": panes}, nil
}

// cwdLock locks a path for one plugin operation (owner + key). It refuses while a pending or
// open worker runs in the path, or has an unknown cwd (a row from before cwd was recorded).
func (h *Hub) cwdLock(ctx context.Context, owner string, args map[string]any) (any, error) {
	path, key, err := lockPathArg(args)
	if err != nil {
		return nil, err
	}
	// Rex calls stay outside the transaction (the store has one connection).
	panes, err := h.liveCwds(ctx, path)
	if err != nil {
		return nil, err
	}
	return h.lockPath(ctx, owner, path, key, panes)
}

// lockPath is the durable part of cwdLock: panes are the live panes in the path (refuse if any).
func (h *Hub) lockPath(ctx context.Context, owner, path, key string, panes []any) (any, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// An existing lock on the path, a parent or a descendant: a retry of the same operation on the
	// same path succeeds; any other overlap is a conflict, so two operations never cross guards.
	locks, err := tx.QueryContext(ctx, `SELECT path, owner, key FROM cwd_locks`)
	if err != nil {
		return nil, err
	}
	for locks.Next() {
		var held, heldOwner, heldKey string
		if err := locks.Scan(&held, &heldOwner, &heldKey); err != nil {
			locks.Close()
			return nil, err
		}
		if !under(path, held) && !under(held, path) {
			continue
		}
		locks.Close()
		if held == path && heldOwner == owner && heldKey == key {
			return map[string]any{"locked": true, "path": path}, nil
		}
		return nil, bad("conflict: %s overlaps %s, locked by %s (operation %s)", path, held, heldOwner, heldKey)
	}
	locks.Close()
	workers, err := cwdWorkers(ctx, tx, path)
	if err != nil {
		return nil, err
	}
	if len(workers) > 0 || len(panes) > 0 {
		return map[string]any{"locked": false, "path": path, "workers": workers, "panes": panes}, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO cwd_locks VALUES (?, ?, ?, ?)`, path, owner, key, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	h.ev.emit("cwd_locked", map[string]any{"path": path, "owner": owner, "key": key})
	return map[string]any{"locked": true, "path": path}, nil
}

// cwdUnlock removes a lock. Only the owning operation (owner + key) may, unless admin is set.
// Idempotent. The path need not exist any more (the worktree is removed by then).
func (h *Hub) cwdUnlock(ctx context.Context, owner string, admin bool, args map[string]any) (any, error) {
	text, _ := args["path"].(string)
	key, _ := args["key"].(string)
	if !filepath.IsAbs(text) || (!admin && key == "") {
		return nil, bad("path (absolute) and key are required")
	}
	// The lock stores the resolved path. After removal the path is gone, so match the stored form.
	path := filepath.Clean(text)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	q, qa := `DELETE FROM cwd_locks WHERE path = ? AND owner = ? AND key = ?`, []any{path, owner, key}
	if admin {
		q, qa = `DELETE FROM cwd_locks WHERE path = ?`, []any{path}
	}
	res, err := h.db.ExecContext(ctx, q, qa...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		h.ev.emit("cwd_unlocked", map[string]any{"path": path, "owner": owner, "admin": admin})
	} else if !admin {
		var held, heldKey string
		if h.db.QueryRowContext(ctx, `SELECT owner, key FROM cwd_locks WHERE path = ?`, path).Scan(&held, &heldKey) == nil {
			return nil, bad("conflict: %s is locked by %s (operation %s)", path, held, heldKey)
		}
	}
	return map[string]any{"locked": false, "path": path}, nil
}

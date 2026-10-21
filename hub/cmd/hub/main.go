// Command hub is the spike 3 LazyRex hub: the spike 2 hub (store, roster, notices, plugin
// lifecycle) plus the UI framework in ui.go. See ../../README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

type Config struct {
	Server string `json:"server"`
	State  string `json:"state"`
	Rex    string `json:"rex"`  // optional path to the rex CLI that launches plugins (default: rex on PATH)
	View   string `json:"view"` // path to the renderer (lazyrex-view) that UI panes run
	// Home is the plugin whose tabs are always mounted first in the LazyRex session (default lazyrex).
	Home string `json:"home"`
	// Profiles is the global profile file (JSON). Empty: no profiles. Spawn points add files later.
	Profiles string `json:"profiles"`
	// PaletteLua is the generated palette file beside the Rex config (lazyrex_palette.lua). Empty:
	// no palette entries.
	PaletteLua string `json:"palette_lua"`
	Real       bool   `json:"-"` // set from -real-server; renderers get -real-server too
	Test       bool   `json:"-"` // set from -test (testmode.go)
	// TestRoots are the directories that test-mode profiles and spawn cwds must stay in (the lab,
	// the plugin and the manifest directories; isolation.go). Only -test accepts it.
	TestRoots []string `json:"test_roots"`
	// TestHoldRecovery holds plugin recovery at hub start until hub.test.recover, so a lab test can
	// call actions before recovery reaches a record. Only -test accepts it.
	TestHoldRecovery bool `json:"test_hold_recovery"`
	// Transcript opens tabs that declare layout "transcript" as a transcript pane over a composer
	// pane (pair.go). Off: those tabs open as one view, as before.
	Transcript bool `json:"transcript"`
	Agents     []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		StatusRecord string `json:"status_record"`
	} `json:"agents"`
	// Entries are the config field plugins (pluginconfig.go). loadConfig checks them into entries,
	// in config order. The hub resolves the entries at each start attempt (resolveEntries).
	Entries []json.RawMessage `json:"plugins"`
	// Lock is the lock file of the GitHub sources (docs/plugin-sources.md, section 3), relative
	// to the config directory. Empty: the default of lockPath. A missing lock file is not an error.
	Lock string `json:"lock"`
	// ManagedPlugins is the managed file of the plugin manager (docs/plugin-sources.md, section 13),
	// relative to the config directory. Default: <state>/plugins.json.
	ManagedPlugins string `json:"managed_plugins"`
	entries        []*configEntry // the config entries, then the managed entries
	managedPath    string         // the resolved managed file
	managedErr     error          // the managed file is malformed: no managed entry
	path           string         // the config file, as given
}

type Agent struct {
	ID, Name, StatusRecord string
	Session                string // the session of BlockID; agents may run in any session
	BlockID                string
	ChildPID               int
	ChildStart             string
	State                  string           // latest state of the trusted record; "" when absent or removed
	stateGen               int              // counts State writes (setState): a status read applies only if none happened during it
	ClientID               string           // the shim connection that joined last
	Profile                string           // set for an agent that a spawn created
	Kind, IdState          string           // an identity (identity.go): its kind and state; "" for a legacy agent
	delivered              map[string]int64 // namespace -> highest intent revision written for this connection
}

// setState records a new program status state. h.mu must be held for an agent that the hub lists.
func (a *Agent) setState(s string) { a.State, a.stateGen = s, a.stateGen+1 }

func (a *Agent) live() bool { return a.BlockID != "" && alive(a.ChildPID, a.ChildStart) }

// ready reports whether the hub may write a notice. A notice written while the agent is working
// steers the running turn (decision "Agent status comes from Rex program status"). blocked means
// a prompt waits for the user: Enter could answer it. error and an absent record are unknown
// states.
func (a *Agent) ready() bool {
	return a.live() && (a.State == "idle" || a.State == "done" || a.State == "working")
}

// Plugin is the record of one plugin name (docs/plugin-sources.md, section 7). h.mu guards its
// fields; lifecycle serializes its starts and stops.
type Plugin struct {
	Name       string
	entry      *configEntry // the entry that bound this name; nil for a kept record (loadKept)
	source     string       // the source that the instance ran from (the plugins row)
	run        pluginRun    // the metadata of the recorded instance; zero when there is none (saved in the plugins row)
	Desired    string       // running or stopped
	InstanceID string
	SessionID  string // the session of the instance's block ("" in rows from before it was recorded)
	BlockID    string
	ChildPID   int
	ChildStart string
	ClientID   string   // set by an accepted hub.register
	Exports    []string // action names agents may call through the MCP shim
	// state is "" (running or stopped, from Desired), refused (the last start check refused the
	// entry), retained (refused, and the recorded instance still lives; it may not register) or
	// waiting (the check passed, and a dependency is not registered: waitLoop starts it again).
	state     string
	refusal   string     // the error of the refused start (state refused or retained)
	waiting   []string   // the dependencies that are not registered (state waiting)
	attempted bool       // a start attempt (startPlugin) ran in this hub process: recovery skips the record
	lifecycle sync.Mutex // serializes replace, start and stop
}

// reportedState is the state that hub.plugins reports. h.mu must be held.
func (pl *Plugin) reportedState() string {
	switch {
	case pl.state != "":
		return pl.state
	case pl.Desired == "stopped":
		return "stopped"
	}
	return "running"
}

// clearInstance forgets the recorded instance and its run. h.mu must be held.
func (pl *Plugin) clearInstance() {
	pl.ClientID, pl.SessionID, pl.BlockID, pl.ChildPID, pl.ChildStart, pl.run = "", "", "", 0, "", pluginRun{}
}

type Hub struct {
	ensureMu       sync.Mutex    // hub.session.ensure: one create at a time (sessions.go)
	slots          slotState     // side slot locks (panels.go)
	noticeKick     chan struct{} // wakes noticeLoop (kickNotices)
	spawnKick      chan struct{} // wakes resolveSpawns (kickSpawns)
	waitKick       chan struct{} // wakes waitLoop (kickWaiting)
	profileStamp   string        // profile files as last checked (checkProfiles)
	profileChecked bool
	cfg            Config
	db             *DB
	rex            *rex.Rex
	ev             *eventLog
	faults         *faults
	generation     int64
	clientID       string
	rexBinary      string
	paletteKick    chan struct{} // wakes paletteLoop (kickPalette)
	watches        *watchSvc     // file watches (watchsvc.go)
	paletteLoaded  string        // the palette file content of the last config reload (paletteLoop only)
	lock           *os.File      // holds the flock; kept for the life of the process
	test           *testState    // test mode (-test, testmode.go); nil otherwise

	mu           sync.Mutex
	identMu      sync.Mutex // serializes identity reports (identity.go)
	agents       map[string]*Agent
	agentOrder   []string               // config agents first, then spawned agents in creation order
	agentRex     *rex.Rex               // the agent face (agentface.go)
	agentKick    chan struct{}          // wakes agentKeymapLoop
	agentForward map[string]string      // plugin action -> plugin client, as last declared on the agent face
	forwards     map[string]map[int]int // plugin client -> requester pid -> hub.forward calls in flight (agentface.go)
	idents       map[string]*Agent      // conversation identities ("claude:<id>"), mirrored from the store (identity.go)
	aliases      map[string]string      // legacy agent ID -> the identity that took it over
	plugins      map[string]*Plugin     // by name: one record for each bound entry name
	// entries are the config entries (pluginconfig.go), in config order, then the managed entries.
	// entriesMu guards the fields that a resolution or a reload changes. Lock order: reloadMu, then
	// a plugin's lifecycle, then entriesMu, then mu. No code takes a lifecycle lock while it holds
	// entriesMu or mu. pl.entry changes only under entriesMu and then mu.
	reloadMu     sync.Mutex // serializes hub.plugins.reload and hub start recovery
	// procRead is the process information read of the caller guards (callerInManagedPane). nil:
	// procRead. A test sets its own Hub's reader; no test changes a package variable.
	procRead func(pid int) (procRecord, error)
	entriesMu    sync.Mutex
	entries      []*configEntry
	managedErr   error // the managed file is malformed (h.entriesMu): hub.plugins reports it once
	selfClosed   map[string]bool // blocks the hub closed itself; their removal is not a user stop
	titleRenames map[string]bool // sessions with a pending identityRenamesSoon
	ui           *uiState
	vs           *viewSvc
}

func isRealServer(s string) bool {
	return strings.Contains(s, "Application%20Support/rex/server.sock") || strings.Contains(s, "Application Support/rex/server.sock")
}

func loadConfig(path string, realServer bool) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Server == "" || c.State == "" || len(c.Agents) == 0 {
		return c, fmt.Errorf("%s: server, state and agents are required", path)
	}
	// The hub keeps its own LazyRex session (workSession). A configured session ID went stale
	// when that session closed, so the field is refused, not ignored.
	var keys map[string]json.RawMessage
	json.Unmarshal(b, &keys)
	if _, ok := keys["session"]; ok {
		return c, fmt.Errorf("%s: remove \"session\": the hub uses its own LazyRex session", path)
	}
	// Hard rule (AGENTS.md): never experiment on the user's real Rex server. The one approved
	// exception (the official hub run) must say so with -real-server.
	if isRealServer(c.Server) != realServer {
		if realServer {
			return c, errors.New("-real-server is set, but the config does not name the real Rex server")
		}
		return c, errors.New("refusing the user's real Rex server without -real-server; use a lab server (spikes/lab/server.sh)")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return c, err
	}
	entries, err := parseEntries(filepath.Dir(abs), c.Entries)
	if err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	c.managedPath = filepath.Join(c.State, "plugins.json")
	if c.ManagedPlugins != "" {
		c.managedPath = c.ManagedPlugins
		if !filepath.IsAbs(c.managedPath) {
			c.managedPath = filepath.Join(filepath.Dir(abs), c.managedPath)
		}
	}
	managed, err := parseManaged(c.managedPath)
	if err != nil {
		// A defect of the manager must not stop the plugins that the user configured (section 13).
		c.managedErr = fmt.Errorf("managed plugin file %s: %w", c.managedPath, err)
		managed = nil
	}
	c.entries, c.path = addManaged(entries, managed), path
	if c.View != "" {
		if c.View, err = filepath.Abs(c.View); err != nil {
			return c, err
		}
	}
	return c, nil
}

// pluginArgKey is the form of a plugin arg key. The hub sets pluginArgReserved itself.
var pluginArgKey = regexp.MustCompile(`^[a-z0-9_]+$`)
var pluginArgReserved = map[string]bool{"hub_client_id": true, "instance_id": true, "plugin_session": true}

// hubActionNames are the hub's own client actions, beside hubToolActions and the test actions.
var hubActionNames = []string{"hub.register", "hub.identify", "hub.agents", "hub.agents.resolve", "hub.join", "hub.tools",
	"hub.store.get", "hub.store.scan", "hub.store.batch", "hub.store.op", "hub.admin.start", "hub.admin.stop", "hub.admin.rebind",
	"hub.ui.tabs", "hub.ui.navigate", "hub.ui.reload", "hub.ui.describe", "hub.view.attach", "hub.view.subscribe", "hub.view.get", "hub.view.history",
	"hub.view.action", "hub.view.draft.save", "hub.view.send", "hub.view.operation", "hub.view.effect.retry", "hub.view.seen", "hub.view.close", "hub.session.list", "hub.session.ensure",
	"hub.plugins", "hub.plugins.reload", "hub.digest", "hub.navigate.worker", "hub.navigate.agent", "hub.cwd.check", "hub.cwd.lock", "hub.cwd.unlock", "hub.admin.unlock", "hub.ui.invalidate", "hub.identity.report", "hub.identity.list", "hub.palette.run", "hub.view.release_confirm", "hub.view.replacement", "hub.view.app_action", "hub.app.select_session", "hub.spawn_window", "hub.spawn.spec", "hub.grant.create", "hub.grant.get", "hub.grant.revoke",
	"hub.watch.ensure", "hub.watch.read", "hub.watch.ack", "hub.watch.remove", "hub.watch.list"}

// installedRex is the CLI inside the Rex app bundle (build 1203 layout). A Rex app update moved it
// from Helpers/Rex Server.app, so panes started before the update can have a stale PATH entry.
const installedRex = "/Applications/Rex Beta.app/Contents/Helpers/Rex Server.rex/Contents/MacOS/rex"

func findRex(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	if p, err := exec.LookPath("rex"); err == nil {
		return p, nil
	}
	if _, err := os.Stat(installedRex); err == nil {
		return installedRex, nil
	}
	return "", errors.New("no rex CLI: set \"rex\" in the hub config")
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "test":
			os.Exit(testMain(os.Args[2:]))
		case "testview":
			os.Exit(testViewMain(os.Args[2:]))
		case "testagent":
			os.Exit(testAgentMain(os.Args[2:]))
		case "plugin":
			os.Exit(pluginCmdMain(os.Args[2:], os.Stdout, os.Stderr))
		}
	}
	fs := &faults{}
	configPath := flag.String("config", "", "hub configuration JSON (required)")
	flag.Var(fs, "lab-fault", "lab-only fault, for example point=store-commit,mode=exit (repeatable)")
	realServer := flag.Bool("real-server", false, "run on the user's real Rex server (approved runs only; no lab faults)")
	testMode := flag.Bool("test", false, "test mode for the Lua test harness: hub.test.* actions (lab only, docs/testing.md)")
	flag.Parse()
	if *realServer && len(fs.list) > 0 {
		log.Fatal("-lab-fault is refused with -real-server")
	}
	if *realServer && *testMode {
		log.Fatal("-test is refused with -real-server")
	}
	if *configPath == "" {
		flag.Usage()
		os.Exit(2)
	}
	cfg, err := loadConfig(*configPath, *realServer)
	if err != nil {
		log.Fatal(err)
	}
	cfg.Real = *realServer
	cfg.Test = *testMode
	if !cfg.Test && len(cfg.TestRoots) > 0 {
		log.Fatal("test_roots needs -test")
	}
	if !cfg.Test && cfg.TestHoldRecovery {
		log.Fatal("test_hold_recovery needs -test")
	}
	if cfg.Test && len(cfg.TestRoots) == 0 {
		log.Fatal("-test needs test_roots in the config (lazyrex-hub test writes it)")
	}
	if cfg.Test {
		if err := checkLabProof(cfg.Server); err != nil {
			log.Fatal(err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h, err := start(ctx, cfg, fs)
	if err != nil {
		log.Fatal(err)
	}
	h.run(ctx)
}

// start takes the lock, opens the store and connects to Rex. Clients find the hub by its label (target.resolve).
func start(ctx context.Context, cfg Config, fs *faults) (*Hub, error) {
	if err := os.MkdirAll(cfg.State, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(cfg.State, "hub.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// Exclusive ownership: a second hub on this state directory stops here and changes nothing.
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("another hub owns %s: %w", cfg.State, err)
	}

	db, err := openDB(filepath.Join(cfg.State, "hub.db"))
	if err != nil {
		return nil, err
	}
	gen, err := db.newGeneration()
	if err != nil {
		return nil, err
	}
	ev, err := openEvents(filepath.Join(cfg.State, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	ev.generation = revString(gen)
	rexBinary, err := findRex(cfg.Rex)
	if err != nil {
		return nil, err
	}
	socket, err := rex.SocketPath(cfg.Server)
	if err != nil {
		return nil, err
	}
	r, err := rex.Dial(ctx, socket)
	if err != nil {
		return nil, err
	}
	var welcome struct {
		Client struct {
			ID string `json:"id"`
		} `json:"client"`
	}
	if err := json.Unmarshal(r.Welcome, &welcome); err != nil || welcome.Client.ID == "" {
		return nil, fmt.Errorf("welcome has no client id: %s", r.Welcome)
	}
	h := &Hub{noticeKick: make(chan struct{}, 1), paletteKick: make(chan struct{}, 1), spawnKick: make(chan struct{}, 1), waitKick: make(chan struct{}, 1), cfg: cfg, db: db, rex: r, ev: ev, faults: fs, generation: gen, clientID: welcome.Client.ID, rexBinary: rexBinary, lock: lock,
		agents: map[string]*Agent{}, idents: map[string]*Agent{}, aliases: map[string]string{}, plugins: map[string]*Plugin{}, selfClosed: map[string]bool{}}
	for _, a := range cfg.Agents {
		h.agents[a.ID] = &Agent{ID: a.ID, Name: a.Name, StatusRecord: a.StatusRecord, delivered: map[string]int64{}}
		h.agentOrder = append(h.agentOrder, a.ID)
	}
	h.entries, h.managedErr = cfg.entries, cfg.managedErr
	if h.managedErr != nil {
		log.Printf("%v: every managed entry is refused", h.managedErr)
	}
	if err := h.loadKept(ctx); err != nil {
		return nil, err
	}
	h.entriesMu.Lock()
	h.resolveLocked() // binds the names and makes their records
	for _, e := range h.entries {
		if e.err != nil {
			log.Printf("plugin %s (%s): refused: %v", e.identity(), e.Source, e.err)
		}
	}
	h.entriesMu.Unlock()
	if cfg.Test {
		h.test = newTestState()
	}

	var actions []map[string]any
	for _, name := range hubActionNames {
		actions = append(actions, map[string]any{"name": name, "kind": "client", "title": name})
	}
	actions = append(actions, hubToolActions...)
	if h.test != nil {
		for _, name := range testActions {
			actions = append(actions, map[string]any{"name": name, "kind": "client", "title": name})
		}
	}
	if err := r.Call(ctx, "client.keymap", map[string]any{"actions": actions}, nil); err != nil {
		return nil, err
	}
	// Set the label after the actions exist: clients find the hub by this label and call at once.
	if err := r.Call(ctx, "client.set_label", map[string]any{"label": "lazyrex-hub"}, nil); err != nil {
		return nil, err
	}
	// uiStart attaches to the LazyRex session (the work session) before bindings are read, so no
	// change is missed between the two.
	if err := h.uiStart(ctx); err != nil {
		return nil, err
	}
	if err := h.workersLoad(ctx); err != nil {
		return nil, err
	}
	if err := h.loadBindings(ctx); err != nil {
		return nil, err
	}
	if err := h.identitySync(ctx); err != nil {
		return nil, err
	}
	go h.uiWorker(context.Background())
	h.kickUI("")
	if err := h.viewsStart(ctx); err != nil {
		return nil, err
	}
	h.workersRun(ctx)
	if err := h.watchStart(ctx); err != nil {
		return nil, err
	}
	h.formsSweep(ctx)
	h.slotSweep(ctx)
	go h.paletteLoop(context.Background())
	h.kickPalette() // the command set may have changed while the hub was down
	if err := h.agentFaceStart(ctx, socket); err != nil {
		return nil, err
	}
	ev.emit("hub_started", map[string]any{"client_id": h.clientID, "pid": os.Getpid()})
	return h, nil
}

func (h *Hub) run(ctx context.Context) {
	go h.recoverPlugins(ctx)
	go h.waitLoop(ctx)
	go h.noticeLoop(ctx)
	h.kickNotices() // notices pending from before the start
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-h.rex.Events:
			if !ok {
				log.Print("rex connection closed")
				return
			}
			h.handleEvent(ctx, raw)
		}
	}
}

type perform struct {
	CallID    string          `json:"call_id"`
	Action    string          `json:"action"`
	Args      json.RawMessage `json:"args"`
	Requester struct {
		ClientID  string `json:"client_id"`
		Principal struct {
			Kind string `json:"kind"`
			PID  int    `json:"pid"`
		} `json:"principal"`
	} `json:"requester"`
}

func (h *Hub) handleEvent(ctx context.Context, raw json.RawMessage) {
	var env struct {
		Payload struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return
	}
	switch env.Payload.Type {
	case "client_perform_requested":
		var p perform
		if json.Unmarshal(env.Payload.Payload, &p) == nil {
			go h.answer(ctx, p) // the event reader never blocks on a handler
		}
	case "session_destroyed":
		var sd struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(env.Payload.Payload, &sd) == nil {
			go h.uiSessionDestroyed(ctx, sd.SessionID)
			h.kickSpawns()
		}
	case "session_event":
		var se struct {
			SessionID string `json:"session_id"`
			Event     struct {
				Type    string `json:"type"`
				Payload struct {
					BlockID  string          `json:"block_id"`
					WindowID string          `json:"window_id"` // window_label_changed {window_id, label} (Rex 1203, lab)
					Label    string          `json:"label"`
					Name     string          `json:"name"`
					Payload  json.RawMessage `json:"payload"`
				} `json:"payload"`
			} `json:"event"`
		}
		if json.Unmarshal(env.Payload.Payload, &se) != nil {
			return
		}
		switch se.Event.Type {
		case "block_event":
			h.blockEvent(ctx, se.Event.Payload.BlockID, se.Event.Payload.Name, se.Event.Payload.Payload)
			if se.Event.Payload.Name == "title_changed" {
				h.identityRenamesSoon(ctx, se.SessionID) // names of unnamed tabs follow the title
			}
		case "block_closed":
			// A closed pane sends block_closed and no child_exited; a process exit in a
			// kept-open pane sends child_exited only (lab, Rex 0.0.1792088193-g7a93ec).
			go h.blockClosed(ctx, se.Event.Payload.BlockID)
			go h.identityPaneClosed(ctx, se.Event.Payload.BlockID)
			h.kickSpawns()
		case "block_created":
			h.kickSpawns() // a pending spawn's pane may have appeared
		case "session_view_changed":
			h.uiSessionChanged(se.SessionID)
		case "window_closed":
			h.forgetWindowLabels(se.Event.Payload.WindowID)
		case "session_label_changed":
			h.sessionRenamed(ctx, se.SessionID, se.Event.Payload.Label) // in event order: database writes only
		case "window_label_changed":
			go h.identityRenames(ctx, se.SessionID)
			h.queueLabelEvent(ctx, se.SessionID, se.Event.Payload.WindowID, se.Event.Payload.Label)
		}
	}
}

func (h *Hub) blockEvent(ctx context.Context, blockID, name string, payload json.RawMessage) {
	switch name {
	case "child_exited":
		go h.identityChildExited(ctx, blockID)
		if v, _ := h.viewByBlock(ctx, blockID); v != nil {
			go h.uiChildExited(ctx, blockID) // a renderer, not a plugin
			return
		}
		if h.transcriptChildExited(ctx, blockID) {
			return
		}
		h.mu.Lock()
		var exited *Plugin
		for _, p := range h.plugins {
			if p.BlockID == blockID {
				exited = p
			}
		}
		h.mu.Unlock()
		if exited != nil {
			go h.pluginExited(ctx, exited, blockID, payload)
		} else if h.isWorkerBlock(ctx, blockID) {
			go h.changed("workers") // the home shows the worker as exited
		}
	case "program_status_changed", "program_status_removed":
		if h.isWorkerBlock(ctx, blockID) {
			go h.changed("workers")
		}
		var ev struct {
			ID     string   `json:"id"`
			IDs    []string `json:"ids"`
			Record struct {
				State string `json:"state"`
			} `json:"record"`
		}
		json.Unmarshal(payload, &ev)
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, a := range h.everyAgent() {
			if a.BlockID != blockID {
				continue
			}
			// Only the configured record counts. A removed record is not ready.
			if name == "program_status_changed" && ev.ID == a.StatusRecord {
				a.setState(ev.Record.State)
			} else if name == "program_status_removed" && contains(ev.IDs, a.StatusRecord) {
				a.setState("")
			} else {
				continue
			}
			log.Printf("agent %s status %q (record %q)", a.ID, a.State, a.StatusRecord)
			go h.changed("agents") // the home shows agent readiness; go: h.mu is held
		}
	}
}

func (h *Hub) answer(ctx context.Context, p perform) {
	result, err := h.dispatchFaulted(ctx, p)
	reply := map[string]any{"call_id": p.CallID}
	if err != nil {
		var se *storeError
		code := "failed"
		if errors.As(err, &se) {
			code = "refused"
			if se.code != "" {
				code = se.code
			}
		}
		reply["error"] = &rex.RexError{Code: code, Message: err.Error()}
	} else {
		reply["result"] = result
	}
	if err := sendReply(ctx, h.rex, p.Action, reply); err != nil { // replyguard.go
		log.Printf("reply %s %s: %v", p.Action, p.CallID, err)
	}
	log.Printf("%s from %s pid %d -> %v", p.Action, p.Requester.ClientID, p.Requester.Principal.PID, err)
}

func (h *Hub) pluginByClient(clientID string) *Plugin {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.plugins {
		if clientID != "" && p.ClientID == clientID {
			return p
		}
	}
	return nil
}

// roster lists every valid recipient: legacy agents, every identity in any state (a superseded
// identity keeps its mailbox), and aliased legacy IDs.
func (h *Hub) roster() map[string]bool {
	m := map[string]bool{}
	for id := range h.agents {
		m[id] = true
	}
	for id := range h.idents {
		m[id] = true
	}
	return m
}

// everyAgent returns the legacy agents and the identities. h.mu must be held.
func (h *Hub) everyAgent() []*Agent {
	out := make([]*Agent, 0, len(h.agents)+len(h.idents))
	for _, a := range h.agents {
		out = append(out, a)
	}
	for _, a := range h.idents {
		out = append(out, a)
	}
	return out
}

func (h *Hub) dispatch(ctx context.Context, p perform) (any, error) {
	var args map[string]any
	if len(p.Args) > 0 && string(p.Args) != "null" {
		if err := json.Unmarshal(p.Args, &args); err != nil {
			return nil, bad("args must be an object")
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	pid := p.Requester.Principal.PID
	if strings.HasPrefix(p.Action, "hub.test.") {
		if h.test == nil {
			return nil, bad("unknown action %s", p.Action)
		}
		return h.testDispatch(ctx, p, args)
	}

	switch p.Action {
	case "hub.register":
		return h.register(p, args)
	case "hub.join":
		return h.join(ctx, p, args)
	case "hub.agents":
		return h.agentsList(args), nil
	case "hub.agents.resolve":
		return h.resolveAgent(args)
	case "hub.tools":
		return h.tools(), nil
	case "hub.identity.report":
		return h.identityReport(ctx, p, args)
	case "hub.digest":
		return h.digest(p.Requester.ClientID, args)
	case "hub.identity.list":
		return h.identityList(ctx, args)
	case "hub.profiles":
		h.checkProfiles() // no loop: a second check finds the same files
		return h.profilesList(ctx)
	case "hub.spawn":
		return h.spawnWorker(ctx, p, args)
	case "hub.spawn_window":
		return h.spawnWindow(ctx, p, args)
	case "hub.spawn.spec":
		return h.spawnSpec(ctx, p, args)
	case "hub.workers":
		return h.workersList(ctx)
	case "hub.stop":
		return h.stop(ctx, p, args)
	case "hub.navigate.worker":
		return h.navigateWorker(ctx, args)
	case "hub.navigate.agent":
		return h.navigateAgent(ctx, args)
	case "hub.plugins":
		return h.pluginsList(ctx)
	case "hub.plugins.reload":
		return h.reloadAction(ctx, p)
	case "hub.palette.run":
		return h.paletteRun(ctx, p, args)
	case "hub.view.release_confirm":
		return h.releaseConfirm(ctx, p, args)
	case "hub.view.replacement":
		return h.viewReplacement(ctx, p, args)
	case "hub.view.app_action":
		return h.viewAppAction(ctx, p, args)
	case "hub.app.select_session":
		return h.appSelectSession(ctx, p, args)
	case "hub.ui.navigate":
		return h.uiNavigate(ctx, args)
	case "hub.ui.reload":
		return h.uiReload(ctx, args)
	case "hub.ui.describe":
		return h.uiDescribe(ctx, args)
	case "hub.view.attach":
		return h.viewAttach(ctx, p, args)
	case "hub.view.subscribe":
		return h.viewSubscribe(ctx, p, args)
	case "hub.view.get":
		return h.viewGet(ctx, p, args)
	case "hub.view.history":
		return h.viewHistory(ctx, p, args)
	case "hub.view.action":
		return h.viewAdmit(ctx, "action", p, args)
	case "hub.view.draft.save":
		return h.viewAdmit(ctx, "draft", p, args)
	case "hub.view.send":
		return h.viewAdmit(ctx, "send", p, args)
	case "hub.view.seen":
		return h.viewSeen(ctx, p, args)
	case "hub.view.close":
		return h.viewClose(ctx, p, args)
	case "hub.view.effect.retry":
		return h.viewEffectRetry(ctx, p, args)
	case "hub.view.operation":
		return h.viewOperation(ctx, p, args)
	case "hub.admin.start", "hub.admin.stop", "hub.admin.rebind", "hub.admin.unlock":
		if in, err := h.callerInManagedPane(pid); err != nil {
			return nil, unverifiedCaller(err)
		} else if in {
			return nil, bad("admin actions are refused from agent and plugin panes")
		}
		if p.Action == "hub.admin.unlock" {
			return h.cwdUnlock(ctx, "", true, args)
		}
		return h.adminAction(ctx, p.Action, args)
	}

	// The remaining actions are hub services for the current registered plugin instance only.
	pl := h.pluginByClient(p.Requester.ClientID)
	if pl == nil {
		return nil, bad("only a registered plugin instance may call %s", p.Action)
	}
	ns := pl.Name
	switch p.Action {
	case "hub.grant.create":
		return h.grantCreate(ctx, p, ns, args)
	case "hub.grant.get":
		return h.grantGet(ctx, ns, args)
	case "hub.grant.revoke":
		return h.grantRevoke(ctx, ns, args)
	case "hub.watch.ensure", "hub.watch.read", "hub.watch.ack", "hub.watch.remove", "hub.watch.list":
		return h.watchAction(ctx, ns, p.Action, args)
	case "hub.ui.tabs":
		return h.uiTabs(ctx, ns, args)
	case "hub.ui.invalidate":
		// The plugin's own data changed outside the store (for example a scan finished): render its
		// open views again. Fast; the view worker does the render.
		if h.test != nil {
			h.test.mu.Lock()
			h.test.invalidate[ns]++
			h.test.mu.Unlock()
		}
		h.kickViews(ns, true)
		return map[string]any{"status": "accepted"}, nil
	case "hub.cwd.lock":
		return h.cwdLock(ctx, ns, args)
	case "hub.cwd.check":
		return h.cwdCheck(ctx, args)
	case "hub.cwd.unlock":
		return h.cwdUnlock(ctx, ns, false, args)
	case "hub.session.list":
		return h.sessionList(ctx)
	case "hub.session.ensure":
		return h.sessionEnsure(ctx, ns, args)
	case "hub.identify":
		target, _ := args["pid"].(float64)
		h.mu.Lock()
		defer h.mu.Unlock()
		// A pane's current conversation identity comes first; else the legacy joined agent.
		for _, a := range h.idents {
			if a.IdState == "current" && a.live() && descendsFrom(int(target), a.ChildPID) {
				return map[string]any{"found": true, "agent": a.ID, "identity": a.ID, "legacy": h.aliasesOf(a.ID), "block_id": a.BlockID, "session_id": h.bindingSession(a)}, nil
			}
		}
		for _, a := range h.agents {
			if a.live() && descendsFrom(int(target), a.ChildPID) {
				return map[string]any{"found": true, "agent": a.ID, "block_id": a.BlockID, "session_id": h.bindingSession(a)}, nil
			}
		}
		return map[string]any{"found": false}, nil
	case "hub.store.get":
		out, err := h.db.storeGet(ctx, ns, args)
		if err == nil && h.test != nil {
			if err := h.testAfterGet(ctx, ns, args); err != nil {
				return nil, err
			}
		}
		return out, err
	case "hub.store.scan":
		return h.db.storeScan(ctx, ns, args)
	case "hub.store.op":
		return h.db.storeOp(ctx, ns, args)
	case "hub.store.batch":
		if h.test != nil {
			if err := h.testBeforeBatch(ctx, ns, args); err != nil {
				return nil, err
			}
		}
		out, err := h.db.storeBatch(ctx, ns, args, h.roster(), h.noticed(ns))
		if err != nil {
			return nil, err
		}
		if out.committed {
			h.afterCommit(ns, out)
			if f := h.faults.take("store-commit", "", out.opKey); f != nil {
				h.ev.emit("fault", map[string]any{"point": f.point, "mode": f.mode, "op_key": out.opKey})
				if f.mode == "exit" {
					os.Exit(faultExitCode)
				}
				select {} // pause: never reply; the test stops the hub by pid
			}
		}
		return out.result, nil
	}
	return nil, bad("unknown action %s", p.Action)
}

// afterCommit announces a committed batch: the event log, pending notices, and the plugin's views.
func (h *Hub) afterCommit(ns string, out *batchOutcome) {
	fields := map[string]any{"plugin": ns, "revs": out.revs}
	if out.opKey != "" {
		fields["op_key"] = out.opKey
	}
	h.ev.emit("store_committed", fields)
	if len(out.intentRev) > 0 {
		h.kickNotices()
	}
	h.kickViews(ns, true) // the plugin's data changed: render its open views again
}

// callerInManagedPane reports whether pid runs inside an agent or plugin pane. It walks the
// ancestry once, outside h.mu. Three outcomes: a match (true), a complete walk without a match
// (false), and an incomplete walk without a match (an error). A guard that allows on false must
// refuse on the error: a failed process information read never lets a caller pass.
func (h *Hub) callerInManagedPane(pid int) (bool, error) {
	read := h.procRead
	if read == nil {
		read = procRead
	}
	chain, walkErr := ancestryOf(read, pid)
	in := func(c int) bool { return c > 0 && slices.Contains(chain, c) }
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.everyAgent() {
		if a.live() && in(a.ChildPID) {
			return true, nil
		}
	}
	for _, p := range h.plugins {
		if in(p.ChildPID) {
			return true, nil
		}
	}
	// No match in the pids read. With an incomplete walk the answer is unknown: the guard refuses.
	return false, walkErr
}

func (h *Hub) tools() any {
	names := h.pluginNames()
	h.mu.Lock()
	defer h.mu.Unlock()
	var list []any
	for _, a := range hubToolActions {
		name := a["name"].(string)
		list = append(list, map[string]any{"name": strings.ReplaceAll(name, ".", "_"), "action": name, "plugin": "hub", "client_id": h.clientID})
	}
	for _, name := range names {
		p := h.plugins[name]
		if p.ClientID == "" {
			continue
		}
		for _, e := range p.Exports {
			list = append(list, map[string]any{
				"name": strings.ReplaceAll(e, ".", "_"), "action": e, "plugin": p.Name, "client_id": p.ClientID,
			})
		}
	}
	return map[string]any{"tools": list}
}

// Command hub is the spike 3 LazyRex hub: the spike 2 hub (store, roster, notices, plugin
// lifecycle) plus the UI framework in ui.go. See ../../README.md.
package main

import (
	"context"
	"database/sql"
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
	"sync/atomic"
	"syscall"
	"time"

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
	// SDK is the directory of the plugin SDK (sdk/lua: boot.lua and lazyrex.lua), set from -sdk. Empty: a
	// plugin that declares sdk in its manifest cannot start.
	SDK  string `json:"-"`
	Test bool   `json:"-"` // set from -test (testmode.go)
	// TestRoots are the directories that test-mode profiles and spawn cwds must stay in (the lab,
	// the plugin and the manifest directories; isolation.go). Only -test accepts it.
	TestRoots []string `json:"test_roots"`
	// TestHoldRecovery holds plugin recovery at hub start until hub.test.recover, so a lab test can
	// call actions before recovery reaches a record. Only -test accepts it.
	TestHoldRecovery bool `json:"test_hold_recovery"`
	// Transcript opens tabs that declare layout "transcript" as a transcript pane over a composer
	// pane (pair.go). Off: those tabs open as one view, as before.
	Transcript bool `json:"transcript"`
	// UISlot is where forms and panels open, for an experiment (the user, 2026-10-22): "split" (the
	// window's right split, the default) or "layer" (a floating layer in the same window). It goes
	// away when the user decides.
	UISlot string `json:"ui_slot"`
	Agents []struct {
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
	ManagedPlugins string         `json:"managed_plugins"`
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
	Record                 statusRecord     // the trusted record's fields (prompts.go); Record.State == State
	stateGen               int              // counts record writes (setRecord): a status read applies only if none happened during it
	ClientID               string           // the shim connection that joined last
	Profile                string           // set for an agent that a spawn created
	Kind, IdState          string           // an identity (identity.go): its kind and state; "" for a legacy agent
	delivered              map[string]int64 // namespace -> highest intent revision written for this connection (notice_deliveries keeps it across restarts)
}

// setRecord records a new program status record. h.mu must be held for an agent that the hub
// lists. The caller then calls h.syncPrompts: a record change can open or close a prompt.
func (a *Agent) setRecord(r statusRecord) {
	a.State, a.Record = r.State, r
	a.stateGen++
}

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
	Exports    []string // action names agents may call through the agent face
	// state is "" (running or stopped, from Desired), refused (the last start check refused the
	// entry), retained (refused, and the recorded instance still lives; it may not register) or
	// waiting (the check passed, and a dependency is not registered: waitLoop starts it again).
	// hub.plugins reports "" as crashing while a respawn is due (respawn.go).
	state     string
	refusal   string       // the error of the refused start (state refused or retained)
	waiting   []string     // the dependencies that are not registered (state waiting)
	attempted bool         // a start attempt (startPlugin) ran in this hub process: recovery skips the record
	opAttempt string       // the bound op attempt of the running admin call (adminattempt.go); only under lifecycle
	lifecycle sync.Mutex   // serializes replace, start and stop
	writes    sync.RWMutex // the store fence: a store write holds it to read (fencedBatch)
	respawn   respawnState // the crash backoff (respawn.go); h.mu guards it
	commands  commandState // the admitted palette commands (plugins.go); h.mu guards it
}

// reportedState is the state that hub.plugins reports. h.mu must be held.
func (pl *Plugin) reportedState() string {
	switch {
	case pl.state != "":
		return pl.state
	case pl.crashing():
		return "crashing"
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
	rc             remoteControl // the last observed Remote Control result for each app (remotecontrol.go)
	sql            *sqlService   // the plugin SQL databases (sqlsvc.go)
	parts          partSets      // reply parts of plugin clients (replyparts.go)
	ensureMu       sync.Mutex    // hub.session.ensure: one create at a time (sessions.go)
	cmdMu          sync.Mutex    // command saves, one at a time (persistCommands, plugins.go)
	slots          slotState     // side slot locks (panels.go)
	noticeKick     chan struct{} // wakes noticeLoop (kickNotices)
	renudgeAfter   atomic.Int64  // nanoseconds to the one repeat of an unread nudge; 0: noticeRenudgeAfter (hub.test.notice_renudge)
	spawnKick      chan struct{} // wakes resolveSpawns (kickSpawns)
	resumeKick     chan struct{} // wakes resumeLoop (kickResume)
	waitKick       chan struct{} // wakes waitLoop (kickWaiting)
	profileStamp   string        // profile files as last checked (checkProfiles)
	profileChecked bool
	cfg            Config
	db             *DB
	rex            *rex.Rex
	ev             *eventLog
	faults         *faults
	generation     int64
	prompts        promptBook               // open prompts (prompts.go); h.mu guards it
	promptSenders  map[string]*promptSender // plugin -> the prompt sender of its registration (promptpush.go)
	promptLocks    promptAnswerLocks        // one answer at a time per agent (promptanswer.go)
	clientID       string
	rexBinary      string
	paletteKick    chan struct{} // wakes paletteLoop (kickPalette)
	watches        *watchSvc     // file watches (watchsvc.go)
	paletteLoaded  string        // the palette file content of the last config reload (paletteLoop only)
	lock           *os.File      // holds the flock; kept for the life of the process
	test           *testState    // test mode (-test, testmode.go); nil otherwise

	// The palette loop and its stop (forms.go, paletteStop). paletteLoop owns paletteLoaded and
	// paletteMine until paletteDone closes; then paletteStop owns them.
	paletteCancel context.CancelFunc // ends paletteLoop; nil before startPalette
	paletteDone   chan struct{}      // closed when paletteLoop returns
	paletteTried  chan struct{}      // closed after the first writePalette (the lab fault palette-start)
	paletteMine   string             // the content that this hub put in the file, or found there
	paletteOnce   sync.Once          // paletteStop runs once
	paletteHook   func()             // tests only: runs before a file is published; nil in production
	// shutdown cancels main's signal context, as SIGTERM does (hub.admin.shutdown). nil: no stop.
	shutdown func()
	// The restart wrapper (relaunch.go): exec is the wrapper's exec count (0 without a wrapper),
	// wrapped is true after a full ready line to a wrapper, and relaunching is the latch of an
	// accepted hub.admin.relaunch.
	exec        int
	wrapped     atomic.Bool
	relaunching atomic.Bool
	calls    *callLog // the per-call lines of answer (calllog.go); nil: a line for every call
	// The crash backoff clock (respawn.go). nil: time.Now and time.AfterFunc; a test sets fakes.
	now       func() time.Time
	afterFunc func(time.Duration, func()) stopper

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
	reloadMu sync.Mutex // serializes hub.plugins.reload and hub start recovery
	// procRead is the process information read of the caller guards (callerInManagedPane). nil:
	// procRead. A test sets its own Hub's reader; no test changes a package variable.
	procRead     func(pid int) (procRecord, error)
	entriesMu    sync.Mutex
	entries      []*configEntry
	managedErr   error           // the managed file is malformed (h.entriesMu): hub.plugins reports it once
	selfClosed   map[string]bool // blocks the hub closed itself; their removal is not a user stop
	titleRenames map[string]bool // sessions with a pending identityRenamesSoon
	ui           *uiState
	vs           *viewSvc

	// unsentSpawns: spawn ID -> the not_created reason that markSending could not store (h.mu).
	// resolveNext stores it later.
	unsentSpawns map[string]string

	// pops is the plugin manager's op service (pluginops.go).
	pops pluginOpsSvc

	changedQ changedQueue // hub.changed deliveries in flight (changed.go)
}

func loadConfig(path string) (Config, error) {
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
	if c.UISlot != "" && c.UISlot != "split" && c.UISlot != "layer" {
		return c, fmt.Errorf("%s: ui_slot is \"split\" or \"layer\", not %q", path, c.UISlot)
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
var pluginArgReserved = map[string]bool{"hub_client_id": true, "instance_id": true, "plugin_session": true, "main": true, "lazyrex_sdk": true}

// hubActionNames are the hub's own client actions, beside hubToolActions and the test actions.
var hubActionNames = []string{"hub.register", "hub.identify", "hub.agents", "hub.agents.resolve", "hub.join",
	"hub.store.get", "hub.store.scan", "hub.store.batch", "hub.store.op", "hub.store.ops_scan", "hub.admin.start", "hub.admin.stop", "hub.admin.replace", "hub.admin.rebind", "hub.admin.shutdown", "hub.admin.relaunch", "hub.admin.resume", "hub.admin.restart", "hub.admin.restart.form", "hub.admin.sql_return", "hub.info",
	"hub.ui.tabs", "hub.ui.navigate", "hub.ui.reload", "hub.ui.describe", "hub.view.attach", "hub.view.subscribe", "hub.view.get", "hub.view.history",
	"hub.view.action", "hub.view.draft.save", "hub.view.send", "hub.view.operation", "hub.view.effect.retry", "hub.view.seen", "hub.view.close", "hub.session.list", "hub.session.ensure",
	"hub.plugins", "hub.plugins.reload", "hub.plugins.ops", "hub.digest", "hub.navigate.worker", "hub.navigate.agent", "hub.cwd.check", "hub.cwd.lock", "hub.cwd.unlock", "hub.admin.unlock", "hub.ui.invalidate", "hub.identity.report", "hub.identity.list", "hub.palette.run", "hub.view.release_confirm", "hub.view.replacement", "hub.view.app_action", "hub.app.select_session", "hub.app.remote_control", "hub.spawn_window", "hub.spawn.spec", "hub.grant.create", "hub.grant.get", "hub.grant.revoke",
	"hub.watch.ensure", "hub.watch.read", "hub.watch.ack", "hub.watch.remove", "hub.watch.list",
	"hub.prompts", "hub.prompt.answer",
	"hub.sql.begin", "hub.sql.exec", "hub.sql.query", "hub.sql.batch", "hub.sql.commit", "hub.sql.rollback", "hub.sql.replay", "hub.reply.part"}

// hubClientActions are the actions that the hub declares on its own client (the plugin face): its
// own actions, hubToolActions and, in test mode, the test actions. A covered read takes the render
// ID as well (withRenderArg); the agent face declares hubToolActions without it (agentKeymap).
func hubClientActions(test bool) []map[string]any {
	var actions []map[string]any
	for _, name := range hubActionNames {
		actions = append(actions, map[string]any{"name": name, "kind": "client", "title": name})
	}
	for _, a := range hubToolActions {
		if coveredReads[a["name"].(string)] != nil {
			a = withRenderArg(a)
		}
		actions = append(actions, a)
	}
	if test {
		for _, name := range testActions {
			actions = append(actions, map[string]any{"name": name, "kind": "client", "title": name})
		}
	}
	return actions
}

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
	if os.Getenv(labKeeperEnv) == "1" {
		labKeeper() // a lab keeper of lazyrex-hub test (labkeeper.go): it never returns
	}
	fetchChildMain() // a fetch watcher or gate of the plugin manager runs its role and exits (fetchchild.go)
	// The restart wrapper's variables (restartenv.go): read and unset first, before any exec.
	renv, renvErr := takeRestartEnv()
	if renv.readyFD >= 0 {
		catchSIGPIPE() // restartenv.go
	}
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
		case "release":
			os.Exit(releaseMain(os.Args[2:], os.Stdout, os.Stderr))
		case "hook":
			os.Exit(hookMain(os.Args[2:]))
		case "setup":
			os.Exit(setupMain(os.Args[2:]))
		case "stop", "status", "restart":
			os.Exit(ctlMain(os.Args[1], os.Args[2:], os.Stdout, os.Stderr))
		case "version":
			fmt.Println(versionText())
			os.Exit(0)
		}
	}
	hf := hubFlags(flag.CommandLine)
	fs, configPath, realServer, testMode, restart, sdkDir := hf.faults, hf.config, hf.real, hf.test, hf.restart, hf.sdk
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
	// The handler comes before the first call to Rex (checkServer asks Rex for its servers; a server
	// that does not answer holds that call up to 10 s): a signal from here on is a deliberate stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := checkServer(ctx, cfg.Server, *realServer, *testMode); err != nil {
		if ctx.Err() != nil {
			log.Print(err) // a signal during the check is a deliberate stop: exit 0
			return
		}
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
	if renvErr != nil {
		log.Fatal(renvErr)
	}
	if renv.expectState != "" {
		state, err := checkExpectedState(cfg.State, renv.expectState)
		if err != nil {
			log.Print(err)
			os.Exit(exitStateMismatch)
		}
		cfg.State = state
	}
	// The SDK and the view are resolved once, here (findSDK, resolveView): a later switch of a link
	// in either path changes nothing for this hub, its plugin respawns or its renderers. A restart
	// wrapper's child uses the paths that its wrapper resolved (restartenv.go), so every child of
	// one wrapper process has them, also a crash restart after a link switch. This comes after the
	// state check (a child of another state exits 3, and its wrapper stops) and before the -restart
	// branch (the wrapper passes the paths to its children).
	if renv.expectState != "" {
		cfg.SDK, cfg.View = renv.sdk, renv.view
		err = checkPinnedPaths(cfg.SDK, cfg.View)
	} else if cfg.SDK, err = findSDK(*sdkDir); err == nil {
		cfg.View, err = resolveView(cfg.View)
	}
	if err != nil {
		log.Fatal(err)
	}
	if *restart {
		if renv.readyFD >= 0 || renv.expectState != "" {
			log.Fatal("-restart is refused under a restart wrapper")
		}
		os.Exit(restartMain(ctx, cfg, restartArgs(os.Args[1:])))
	}
	if renv.expectState == "" { // not a wrapper's child: a wrapper that owns this state refuses it
		if err := refuseBesideAWrapper(cfg.State); err != nil {
			log.Fatal(err)
		}
	}
	log.Print(sdkLogLine(cfg.SDK)) // here, after the -restart branch: the hub process logs it, not its wrapper
	h, err := start(ctx, cfg, fs)
	if err != nil {
		if ctx.Err() != nil {
			log.Print(err) // a signal during the start is a deliberate stop: exit 0
			return
		}
		log.Fatal(err)
	}
	h.exec = renv.exec
	if err := signalReady(renv.readyFD, h.cfg.State); err != nil {
		log.Print(err) // the hub runs on; the wrapper sees no ready line
	} else if renv.readyFD >= 0 && renv.expectState != "" {
		h.wrapped.Store(true) // the ready handshake succeeded: a wrapper waits for this hub
	}
	h.shutdown = stop
	h.run(ctx)
	h.paletteStop() // no hub entry stays in the palette after the hub ends; the exit status does not change
	h.sql.close()   // open handler transactions roll back
	if h.relaunching.Load() {
		os.Exit(exitRelaunch) // the wrapper execs itself (restartexec.go)
	}
	// Exit status (docs/hub.md, "Stop and exit status"): 0 after a signal or hub.admin.shutdown, 1
	// when the Rex connection closed (h.run logged it) with the context still live.
	if ctx.Err() == nil {
		os.Exit(1)
	}
}

// start takes the lock, opens the store and connects to Rex. Clients find the hub by its label (target.resolve).
func start(ctx context.Context, cfg Config, fs *faults) (*Hub, error) {
	if err := os.MkdirAll(cfg.State, 0o700); err != nil {
		return nil, err
	}
	// Every later path uses the canonical state (restartenv.go), also hub.info and the ready line.
	state, err := canonicalState(cfg.State)
	if err != nil {
		return nil, err
	}
	cfg.State = state
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
	h := &Hub{noticeKick: make(chan struct{}, 1), paletteKick: make(chan struct{}, 1), spawnKick: make(chan struct{}, 1), resumeKick: make(chan struct{}, 1), waitKick: make(chan struct{}, 1), cfg: cfg, db: db, rex: r, ev: ev, faults: fs, generation: gen, clientID: welcome.Client.ID, rexBinary: rexBinary, lock: lock, calls: newCallLog(),
		agents: map[string]*Agent{}, idents: map[string]*Agent{}, aliases: map[string]string{}, plugins: map[string]*Plugin{}, selfClosed: map[string]bool{}}
	h.sql = newSQLService(cfg.State, h.currentClient)
	h.sql.committed = h.markSQL // the views that read a changed table, and every view without a table set
	h.sql.rendered = h.renderRead
	h.sql.replyParts = func(client string, stub map[string]any) ([]byte, error) { return h.parts.join(client, stub, true) }
	h.sql.admitNotices = h.admitSQLNotices
	h.sql.applyNotices = func(plugin string, rows []outboxRow) error {
		return h.db.applySQLNotices(context.Background(), plugin, rows)
	}
	h.sql.dropReceipts = func(plugin string, seq int64) error {
		return h.db.dropNoticeReceipts(context.Background(), plugin, seq)
	}
	h.sql.notified = h.kickNotices
	h.sql.returning = h.db.sqlReturning
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
		h.sql.beforeCommit = h.testBeforeSQLCommit
		h.sql.beforeBegin = h.testBeforeSQLBegin
	}

	if err := r.Call(ctx, "client.keymap", map[string]any{"actions": hubClientActions(h.test != nil)}, nil); err != nil {
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
	if err := h.pluginOpsStart(ctx); err != nil {
		return nil, err
	}
	h.workersRun(ctx)
	if err := h.watchStart(ctx); err != nil {
		return nil, err
	}
	if err := h.promptAnswersStart(ctx); err != nil {
		return nil, err
	}
	h.formsSweep(ctx)
	h.slotSweep(ctx)
	h.startPalette() // the command set may have changed while the hub was down
	if f := fs.take("palette-start", "fail", ""); f != nil {
		<-h.paletteTried
		ev.emit("fault", map[string]any{"point": f.point, "mode": f.mode})
		h.paletteStop()
		return nil, errors.New("lab fault palette-start: the start fails after the first palette write")
	}
	if err := h.agentFaceStart(ctx, socket); err != nil {
		h.paletteStop() // an exit after the first palette write (docs/hub.md, "Stop and exit status")
		return nil, err
	}
	ev.emit("hub_started", map[string]any{"client_id": h.clientID, "pid": os.Getpid()})
	return h, nil
}

func (h *Hub) run(ctx context.Context) {
	go h.recoverPlugins(ctx)
	go h.waitLoop(ctx)
	go h.noticeLoop(ctx)
	go h.resumeLoop(ctx)
	h.kickNotices() // notices pending from before the start
	go func() {
		h.sql.drainAll(ctx) // hub.sql outboxes that a crash or a failed apply left
		h.kickNotices()
	}()
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
			h.tabReplCreated(se.SessionID, se.Event.Payload.BlockID) // before a close or exit event of the block
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
		h.mu.Lock()
		h.syncPrompts() // a legacy agent's process is gone: its prompt closes
		h.mu.Unlock()
		if v, _ := h.viewByBlock(ctx, blockID); v != nil {
			go h.uiChildExited(ctx, blockID) // a renderer, not a plugin
			return
		}
		go h.tabReplPaneExited(ctx, blockID) // a reopen's new renderer before its adoption
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
		} else {
			log.Printf("child_exited for block %s: no plugin or worker has it", blockID)
		}
	case "program_status_changed", "program_status_removed":
		if h.isWorkerBlock(ctx, blockID) {
			go h.changed("workers")
		}
		var ev struct {
			ID     string       `json:"id"`
			IDs    []string     `json:"ids"`
			Record statusRecord `json:"record"`
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
				a.setRecord(ev.Record)
			} else if name == "program_status_removed" && contains(ev.IDs, a.StatusRecord) {
				a.setRecord(statusRecord{})
			} else {
				continue
			}
			h.syncPrompts()
			log.Printf("agent %s status %q (record %q)", a.ID, a.State, a.StatusRecord)
			go h.changed("agents") // the home shows agent readiness; go: h.mu is held
		}
	}
}

func (h *Hub) answer(ctx context.Context, p perform) {
	t0 := time.Now()
	if pl := h.pluginByClient(p.Requester.ClientID); pl != nil {
		h.sql.touch(pl.Name, p.Requester.ClientID) // any hub call keeps an open SQL transaction of the instance
	}
	result, err := h.dispatchFaulted(ctx, p)
	reply := map[string]any{"call_id": p.CallID}
	if err != nil {
		var se *storeError
		code := "failed"
		if errors.As(err, &se) {
			code = storeErrorCode(se)
		}
		reply["error"] = &rex.RexError{Code: code, Message: err.Error()}
	} else {
		reply["result"] = result
	}
	replyErr := sendReply(ctx, h.rex, p.Action, reply) // replyguard.go
	if replyErr != nil {
		log.Printf("reply %s %s: %v", p.Action, p.CallID, replyErr)
	}
	h.calls.record(p.Action, p.Requester.ClientID, p.Requester.Principal.PID, err, replyErr, time.Since(t0)) // calllog.go
	h.afterReply(p.Action, err, replyErr)
}

// afterReply runs what an answered call starts after its reply: hub.admin.shutdown stops the hub,
// only after the reply {"stopping": true} was delivered. A failed reply stops nothing: the caller
// saw an error or a timeout, the hub still runs, and a retry is correct.
func (h *Hub) afterReply(action string, err, replyErr error) {
	if action == "hub.admin.shutdown" && err == nil && replyErr == nil && h.shutdown != nil {
		h.shutdown()
	}
	// An accepted relaunch always runs, also when its reply was lost: a resend with the same
	// expect_exec gets "relaunch in progress" from this hub or "already restarted" from a new one.
	// After a failed exec, the old version runs again at the same exec and accepts it (relaunch.go).
	if action == "hub.admin.relaunch" && err == nil && h.shutdown != nil {
		h.shutdown()
	}
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

func (h *Hub) dispatch(ctx context.Context, p perform) (result any, err error) {
	var args map[string]any
	if len(p.Args) > 0 && string(p.Args) != "null" {
		if err := json.Unmarshal(p.Args, &args); err != nil {
			return nil, bad("args must be an object")
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	// A hub call of a plugin while one of its renders is open can make the render untracked
	// (renderreads.go).
	degraded := false // a covered read whose result may be incomplete (workersRead)
	if pl := h.pluginByClient(p.Requester.ClientID); pl != nil {
		h.renderCall(pl.Name, p.Requester.ClientID, p.Action, args)
		if id, _ := args["render"].(string); id != "" && coveredReads[p.Action] != nil {
			defer func() {
				h.renderHubRead(pl.Name, p.Requester.ClientID, id, coveredReads[p.Action], err == nil && !degraded)
			}()
		}
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
		return h.register(ctx, p, args)
	case "hub.join":
		return h.join(ctx, p, args)
	case "hub.agents":
		return h.agentsList(args), nil
	case "hub.agents.resolve":
		return h.resolveAgent(args)
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
		spawnID, _ := args["spawn_id"].(string)
		result, degraded, err = h.workersRead(ctx, spawnID)
		return result, err
	case "hub.stop":
		return h.stop(ctx, p, args)
	case "hub.navigate.worker":
		return h.navigateWorker(ctx, args)
	case "hub.navigate.agent":
		return h.navigateAgent(ctx, args)
	case "hub.info":
		if len(args) > 0 {
			return nil, bad("hub.info takes no arguments")
		}
		commit, modified := buildCommit()
		return map[string]any{"state": h.cfg.State, "pid": os.Getpid(), "generation": revString(h.generation),
			"exec": h.exec, "commit": commit, "modified": modified, "sdk": h.cfg.SDK, "view": h.cfg.View}, nil
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
	case "hub.app.remote_control":
		if len(args) > 0 {
			return nil, bad("hub.app.remote_control takes no arguments")
		}
		return h.remoteControlRead(ctx)
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
	case "hub.admin.start", "hub.admin.stop", "hub.admin.replace", "hub.admin.rebind", "hub.admin.unlock", "hub.admin.shutdown", "hub.admin.resume", "hub.admin.restart", "hub.admin.restart.form", "hub.admin.sql_return", "hub.admin.relaunch":
		if in, err := h.callerInManagedPane(pid); err != nil {
			return nil, unverifiedCaller(err)
		} else if in {
			return nil, bad("admin actions are refused from agent and plugin panes")
		}
		// After the admin guard: a caller that the guard refuses learns nothing about a relaunch.
		if h.relaunching.Load() {
			return nil, refuse("refused", "relaunch in progress")
		}
		if p.Action == "hub.admin.relaunch" {
			return h.adminRelaunch(args)
		}
		if p.Action == "hub.admin.unlock" {
			return h.cwdUnlock(ctx, "", true, args)
		}
		if p.Action == "hub.admin.resume" {
			return h.adminResume(ctx, args)
		}
		if p.Action == "hub.admin.restart" {
			return h.adminRestart(ctx, args)
		}
		if p.Action == "hub.admin.restart.form" {
			return h.adminRestartForm(ctx, p, args)
		}
		if p.Action == "hub.admin.sql_return" {
			return h.adminSQLReturn(ctx, args)
		}
		if p.Action == "hub.admin.shutdown" {
			// The stop itself starts in answer, after the reply.
			if len(args) > 0 {
				return nil, bad("hub.admin.shutdown takes no arguments")
			}
			return map[string]any{"stopping": true}, nil
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
	case "hub.sql.begin", "hub.sql.exec", "hub.sql.query", "hub.sql.batch", "hub.sql.commit", "hub.sql.rollback", "hub.sql.replay":
		return h.sql.dispatch(ctx, p.Action, sqlCall{plugin: ns, client: p.Requester.ClientID, args: args})
	case "hub.reply.part":
		return h.parts.part(p.Requester.ClientID, args)
	case "hub.plugins.ops":
		// The plugins panel of the home plugin reads the plugin manager's ops (pluginops.go).
		h.mu.Lock()
		home := h.homeRecordLocked(pl.Name)
		h.mu.Unlock()
		if !home {
			return nil, bad("only the home plugin reads the plugin operations")
		}
		var raw map[string]json.RawMessage
		if len(p.Args) > 0 && string(p.Args) != "null" {
			if err := json.Unmarshal(p.Args, &raw); err != nil {
				return nil, bad("args must be an object")
			}
		}
		return h.pluginOpsList(ctx, raw)
	case "hub.grant.create":
		return h.grantCreate(ctx, p, ns, args)
	case "hub.grant.get":
		return h.grantGet(ctx, ns, args)
	case "hub.grant.revoke":
		return h.grantRevoke(ctx, ns, args)
	case "hub.watch.ensure", "hub.watch.read", "hub.watch.ack", "hub.watch.remove", "hub.watch.list":
		return h.watchAction(ctx, ns, p.Action, args)
	case "hub.prompts":
		return h.promptsList(pl)
	case "hub.prompt.answer":
		return h.promptAnswer(ctx, pl, args)
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
		return h.uiInvalidate(ctx, ns, args)
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
		return h.identify(ctx, int(target))
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
	case "hub.store.ops_scan":
		return h.db.storeOpsScan(ctx, ns, args)
	case "hub.store.batch":
		out, err := h.fencedBatch(ctx, pl, p.Requester.ClientID, args)
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

// identify handles hub.identify {pid}. spawn_id is the agent spawn whose open block is the found
// pane. It is read after h.mu is free: it is a fact at read time, like every other field.
func (h *Hub) identify(ctx context.Context, target int) (map[string]any, error) {
	out := h.identifyPID(target)
	if block, _ := out["block_id"].(string); block != "" {
		var spawn string
		err := h.db.QueryRowContext(ctx, `SELECT spawn_id FROM spawns WHERE block_id = ? AND status = 'open' AND agent_id <> ''`, block).Scan(&spawn)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if spawn != "" {
			out["spawn_id"] = spawn
		}
	}
	return out, nil
}

// identifyPID finds the agent of a process for hub.identify: a pane's current conversation
// identity comes first; else the legacy joined agent.
func (h *Hub) identifyPID(target int) map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.idents {
		if a.IdState == "current" && a.live() && descendsFrom(target, a.ChildPID) {
			return map[string]any{"found": true, "agent": a.ID, "identity": a.ID, "legacy": h.aliasesOf(a.ID), "block_id": a.BlockID, "session_id": h.bindingSession(a)}
		}
	}
	for _, a := range h.agents {
		if a.live() && descendsFrom(target, a.ChildPID) {
			return map[string]any{"found": true, "agent": a.ID, "block_id": a.BlockID, "session_id": h.bindingSession(a)}
		}
	}
	return map[string]any{"found": false}
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
	keys := make([]string, 0, len(out.revs))
	for k := range out.revs {
		keys = append(keys, k)
	}
	h.markCommit(ns, keys) // the views whose declaration matches a committed key render again
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

// storeBatchHook is a test hook of fencedBatch. It runs at stage "check" (after the caller check in
// dispatch, before the fence) and at stage "fenced" (under the fence, after the check again).
var storeBatchHook func(stage string)

// fencedBatch runs a hub.store.batch of the instance client under the store fence: it holds
// pl.writes to read, and it checks again that client is the registered instance. A batch that
// passed the caller check before a retirement is then refused (stale_instance), or the retirement
// waits until it commits.
func (h *Hub) fencedBatch(ctx context.Context, pl *Plugin, client string, args map[string]any) (*batchOutcome, error) {
	if storeBatchHook != nil {
		storeBatchHook("check")
	}
	pl.writes.RLock()
	defer pl.writes.RUnlock()
	h.mu.Lock()
	current := client != "" && pl.ClientID == client
	h.mu.Unlock()
	if !current {
		return nil, refuse("stale_instance", "plugin %s: the caller is no longer the registered instance; nothing was written", pl.Name)
	}
	if storeBatchHook != nil {
		storeBatchHook("fenced")
	}
	// Under the fence: a move to hub.sql notices (admitSQLNotices) holds the fence exclusively, so a
	// store notify commits before the move or sees it.
	if hasNotify(args) {
		if moved, err := h.db.noticesMoved(ctx, pl.Name); err != nil {
			return nil, err
		} else if moved {
			return nil, refuse("notices_moved", "plugin %s sends its notices through hub.sql.commit now", pl.Name)
		}
	}
	if h.test != nil {
		if err := h.testBeforeBatch(ctx, pl.Name, args); err != nil {
			return nil, err
		}
	}
	return h.db.storeBatch(ctx, pl.Name, args, h.roster(), h.noticed(pl.Name))
}

// fenceStoreWrites waits for the store writes that hold pl.writes. Call it after the client ID is
// cleared, without h.mu: after it returns, no write of the old instance commits.
func (h *Hub) fenceStoreWrites(pl *Plugin) {
	pl.writes.Lock()
	pl.writes.Unlock()
}

// sdkLogLine is the hub's start line for the SDK that plugins with sdk use: "sdk: <dir>", or "sdk:
// none" (such plugins cannot start).
func sdkLogLine(dir string) string {
	if dir == "" {
		return "sdk: none"
	}
	return "sdk: " + dir
}

// findSDK returns the SDK directory with every symlink resolved, so a later switch of a link
// changes nothing for this hub. The -sdk value must resolve to a directory with boot.lua, else the
// hub does not start. Without -sdk: sdk/lua in a directory above this binary's resolved path (a
// hub built in its checkout, or versions/<commit>/ of an install); none found is "" (plugins that
// declare sdk cannot start).
func findSDK(flagValue string) (string, error) {
	if flagValue != "" {
		dir, err := filepath.EvalSymlinks(flagValue)
		if err == nil {
			dir, err = filepath.Abs(dir)
		}
		if err != nil {
			return "", fmt.Errorf("-sdk %s cannot be resolved: %v", flagValue, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "boot.lua")); err != nil {
			return "", fmt.Errorf("-sdk %s has no boot.lua", flagValue)
		}
		return dir, nil
	}
	self, err := os.Executable()
	if err == nil {
		// A guard: os.Executable already resolves on macOS and Linux, so no test here proves it.
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return "", nil
	}
	for d := filepath.Dir(self); d != "/" && d != "."; d = filepath.Dir(d) {
		c := filepath.Join(d, "sdk", "lua")
		if _, err := os.Stat(filepath.Join(c, "boot.lua")); err == nil {
			if dir, err := filepath.EvalSymlinks(c); err == nil {
				return dir, nil
			}
		}
	}
	return "", nil
}

// checkPinnedPaths checks the paths that a restart wrapper gave its child: a directory of a version
// that is gone stops the start. The child never resolves them again.
func checkPinnedPaths(sdk, view string) error {
	if sdk != "" {
		if _, err := os.Stat(filepath.Join(sdk, "boot.lua")); err != nil {
			return fmt.Errorf("the restart wrapper's SDK %s: %v", sdk, err)
		}
	}
	if view != "" {
		if _, err := os.Stat(view); err != nil {
			return fmt.Errorf("the restart wrapper's view %s: %v", view, err)
		}
	}
	return nil
}

// resolveView returns the configured renderer path (absolute after loadConfig) with every symlink
// resolved, so a crashed renderer is replaced by the same file. A path that cannot be resolved
// stops the start. "" (no view) stays "".
func resolveView(view string) (string, error) {
	if view == "" {
		return "", nil
	}
	v, err := filepath.EvalSymlinks(view)
	if err != nil {
		return "", fmt.Errorf("view %s cannot be resolved: %v", view, err)
	}
	return v, nil
}

// currentClient reports whether client is the registered instance of plugin.
func (h *Hub) currentClient(plugin, client string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	pl := h.plugins[plugin]
	return pl != nil && client != "" && pl.ClientID == client
}

// admitSQLNotices checks the notices of a hub.sql commit with the rules of a notify op of
// hub.store.batch, then records that the plugin's notices moved to hub.sql. The move holds the
// store fence exclusively: a store notify in flight commits first, and a later one is refused
// (notices_moved), so no store notice lands after the first SQL notice.
func (h *Hub) admitSQLNotices(plugin string, notes []noticeOp) error {
	if !h.noticed(plugin) {
		return refuse("no_notice", "plugin %s has no notice in its manifest", plugin)
	}
	h.mu.Lock()
	roster := h.roster()
	pl := h.plugins[plugin]
	h.mu.Unlock()
	if pl == nil {
		return refuse("no_notice", "plugin %s is not configured", plugin)
	}
	for _, n := range notes {
		if !roster[n.agent] {
			return refuse("invalid_input", "notices: %q is not a known agent", n.agent)
		}
	}
	pl.writes.Lock()
	defer pl.writes.Unlock()
	return h.db.moveNotices(context.Background(), plugin)
}

// hasNotify reports whether a hub.store.batch has a notify op.
func hasNotify(args map[string]any) bool {
	ops, _ := asList(args["ops"])
	for _, o := range ops {
		if m, ok := o.(map[string]any); ok && m["notify"] != nil {
			return true
		}
	}
	return false
}

// hubFlagSet holds the flags of a hub run. sdkN counts the -sdk flags (the last one wins).
type hubFlagSet struct {
	faults              *faults
	config, sdk         *string
	real, test, restart *bool
	sdkN                int
}

// hubFlags defines the flags of a hub run on set. main parses with them, and the release SDK check
// (sdkArgExact) parses a hub argv with them: so the check follows the hub's own flag rules.
func hubFlags(set *flag.FlagSet) *hubFlagSet {
	v := &hubFlagSet{faults: &faults{}, sdk: new(string)}
	v.config = set.String("config", "", "hub configuration JSON (required)")
	set.Var(v.faults, "lab-fault", "lab-only fault, for example point=store-commit,mode=exit (repeatable)")
	v.real = set.Bool("real-server", false, "run on the user's real Rex server (approved runs only; no lab faults)")
	v.test = set.Bool("test", false, "test mode for the Lua test harness: hub.test.* actions (lab only, docs/testing.md)")
	v.restart = set.Bool("restart", false, "run the hub as a child and start it again after a failure (restart.go)")
	set.Func("sdk", "the plugin SDK directory (sdk/lua of the LazyRex checkout); default: sdk/lua above this binary", func(s string) error {
		v.sdkN++
		*v.sdk = s
		return nil
	})
	return v
}

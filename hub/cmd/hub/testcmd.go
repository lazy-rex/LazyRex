package main

// `lazyrex-hub test`: the Lua test harness for plugins (docs/testing.md).
//
// Run in a plugin repository, it reads lazyrex.json and, for each tests/*.lua file:
//  1. starts an isolated Rex server in a new temporary directory, with REX_SERVER set to that
//     server for every child (rex-protocol.md, "Pane environment"), and writes the lab proof;
//  2. starts this binary as the hub (-test) with temporary state and a config that loads the plugin;
//  3. runs the file with `rex do lab/test/runner.lua` and reads its LRT result lines;
//  4. stops everything it started and checks that each recorded process is gone.
// A parity sweep (lab/test/parity.lua) runs first, against its own lab, and a failure fails the run.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// manifest is lazyrex.json in a plugin repository: the runtime fields (sources.go pluginManifest,
// the same rules) and the test section, which only the harness reads.
// testFile is the entry of one file in test.files.
type testFile struct {
	Args map[string]map[string]string `json:"args"`
	// Prompts replaces test.prompts for this file: a list of plugin names, or false for none.
	Prompts filePrompts `json:"prompts"`
}

// filePrompts is test.files.<file>.prompts. set is false when the file has no prompts entry.
type filePrompts struct {
	set   bool
	names []string
}

func (f *filePrompts) UnmarshalJSON(b []byte) error {
	if string(b) == "false" {
		*f = filePrompts{set: true}
		return nil
	}
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return fmt.Errorf("prompts is a list of plugin names, or false")
	}
	*f = filePrompts{set: true, names: names}
	return nil
}

type manifest struct {
	Name    string   `json:"name"`
	Main    string   `json:"main"`
	SDK     string   `json:"sdk"`
	Events  string   `json:"events"`
	Notice  string   `json:"notice"`
	Depends []string `json:"depends"`
	Test    struct {
		// Plugins are the other plugins that the hub loads for the tests: local directories,
		// relative to the manifest directory, each with its own lazyrex.json.
		Plugins       []testPlugin    `json:"plugins"`
		Agents        []manifestAgent `json:"agents"`
		Transcript    bool            `json:"transcript"`
		ParityFixture string          `json:"parity_fixture"` // a Lua file that renders the contextual routes
		ParityRoutes  []string        `json:"parity_routes"`  // contextual routes that the fixture must render
		// Env adds variables to the lab environment (isolation.go). Args are plugin args (hub config
		// plugins[].args), keyed by plugin name. Values may name ${LAB} and ${PLUGIN}.
		Env  map[string]string            `json:"env"`
		Args map[string]map[string]string `json:"args"`
		// Files adds plugin args for one test file, keyed by its base name (as -file): args over
		// Args, for that file's lab only.
		Files map[string]testFile `json:"files"`
		// WatchRoots are plugin watch roots (hub config plugins[].watch_roots), keyed by plugin name.
		// Paths with ${LAB}; the harness creates each one before the hub starts.
		WatchRoots map[string][]string `json:"watch_roots"`
		// Prompts names the plugins that get the prompts grant (hub config plugins[].prompts).
		Prompts []string `json:"prompts"`
		// Profiles is a profile file (relative to the manifest dir) that the harness copies to
		// LAB/profiles.json, the hub's profile file.
		Profiles string `json:"profiles"`
	} `json:"test"`
	mainPath string     // the absolute main file (pluginMain)
	deps     []*testDep // the test.plugins manifests, in manifest order
}

type testPlugin struct {
	Source string `json:"source"`
}

// testDep is a plugin of test.plugins, as its own manifest describes it.
type testDep struct {
	dir, main string // absolute
	m         *pluginManifest
}

type manifestAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// checkNotice checks a manifest notice. The hub pastes it into an agent pane and then sends CR,
// so a control character (a newline) would submit or change the agent's input.
func checkNotice(s string) error {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return errors.New("notice must not contain control characters")
		}
	}
	return nil
}

// loadManifest reads the manifest of the plugin under test (dir is absolute) and each test.plugins
// manifest, with the hub's manifest rules. checkPluginGraph applies the graph rules later, when
// the home plugin is known.
func loadManifest(dir string) (*manifest, error) {
	path := filepath.Join(dir, "lazyrex.json")
	b, err := readManifestBytes(dir)
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := decodeManifest(path, b, &m); err != nil {
		return nil, err
	}
	rt := pluginManifest{Name: m.Name, Main: m.Main, SDK: m.SDK, Events: m.Events, Notice: m.Notice, Depends: m.Depends}
	if err := rt.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m.mainPath, err = pluginMain(dir, m.Main); err != nil {
		return nil, fmt.Errorf("plugin %s: %w", m.Name, err)
	}
	names := map[string]bool{m.Name: true}
	for i, p := range m.Test.Plugins {
		if p.Source == "" || filepath.IsAbs(p.Source) || strings.HasPrefix(p.Source, "~") || strings.HasPrefix(p.Source, "github:") {
			return nil, fmt.Errorf("%s: test.plugins %d: source %q must be a directory relative to the manifest", path, i+1, p.Source)
		}
		d := &testDep{dir: filepath.Join(dir, p.Source)}
		if d.m, d.main, err = readPluginManifest(d.dir); err != nil {
			return nil, fmt.Errorf("%s: test.plugins %d: %w", path, i+1, err)
		}
		if d.m.Name == "lazyrex" {
			return nil, fmt.Errorf("%s: test.plugins %d: the harness adds the home plugin lazyrex itself", path, i+1)
		}
		if names[d.m.Name] {
			return nil, fmt.Errorf("%s: test.plugins %d: the name %s is used twice", path, i+1, d.m.Name)
		}
		names[d.m.Name] = true
		m.deps = append(m.deps, d)
	}
	for _, a := range m.Test.Agents {
		if a.ID == "" || a.ID == "user" {
			return nil, fmt.Errorf("%s: test.agents need an id other than user", path)
		}
	}
	if err := m.checkIsolation(dir); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// homePlugin reads the home plugin's manifest in a LazyRex checkout.
func homePlugin(lazyrex string) (*testDep, error) {
	d := &testDep{dir: filepath.Join(lazyrex, "plugins", "lazyrex")}
	var err error
	if d.m, d.main, err = readPluginManifest(d.dir); err != nil {
		return nil, fmt.Errorf("the home plugin: %w", err)
	}
	if d.m.Name != "lazyrex" {
		return nil, fmt.Errorf("the home plugin in %s is named %s, not lazyrex", d.dir, d.m.Name)
	}
	return d, nil
}

// checkPluginGraph applies the hub's graph rules (sources.go checkGraph) to the plugins of a lab:
// the home plugin (nil when it is the plugin under test), the test.plugins and the plugin under
// test. A refused plugin is a harness error.
func (m *manifest) checkPluginGraph(home *testDep) error {
	var entries []*sourceEntry
	add := func(dir, name string, depends []string) {
		entries = append(entries, &sourceEntry{Index: len(entries) + 1, Source: dir, Name: name, Depends: depends})
	}
	if home != nil {
		add(home.dir, home.m.Name, home.m.Depends)
	}
	for _, d := range m.deps {
		add(d.dir, d.m.Name, d.m.Depends)
	}
	add("", m.Name, m.Depends)
	checkGraph(entries)
	for _, e := range entries {
		if e.Err != nil {
			return e.Err
		}
	}
	return nil
}

type testOpts struct {
	fileArgs                              map[string]map[string]string // test.files args of the file that runs now
	filePrompts                           filePrompts                  // test.files prompts of the file that runs now (promptGrant)
	dir, lazyrex, self, rexBin, run, file string
	home                                  *testDep // the home plugin; nil when it is the plugin under test
	keep, verbose                         bool
	timeout                               time.Duration
	m                                     *manifest
	restartFn                             func(ctx context.Context, l *lab, mode string) error // tests only; default restartHub
}

// fileResult is the outcome of one test file.
type fileResult struct {
	name           string
	passed, failed int
	problems       []string // harness failures: no plan, a crash, cleanup, ...
	labDir         string
	parity         bool
}

func (r *fileResult) ok() bool { return r.failed == 0 && len(r.problems) == 0 }

func testMain(argv []string) int {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	o := testOpts{}
	fs.StringVar(&o.run, "run", "", "run only tests whose name matches this Lua pattern")
	fs.StringVar(&o.file, "file", "", "run only this file of tests/ (a base name, for example send.lua)")
	fs.BoolVar(&o.keep, "keep", false, "keep each lab directory (logs, events, state); processes are stopped anyway")
	fs.BoolVar(&o.verbose, "v", false, "print hub, plugin and runner logs")
	fs.DurationVar(&o.timeout, "timeout", 120*time.Second, "time limit for one test file")
	fs.StringVar(&o.lazyrex, "lazyrex", "", "the LazyRex checkout (default: ../LazyRex beside the plugin, or this binary's checkout)")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "usage: lazyrex-hub test [flags] [plugin dir]")
		return 2
	}
	o.dir = "."
	if fs.NArg() == 1 {
		o.dir = fs.Arg(0)
	}
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "lazyrex test:", err)
		return 2
	}
	var err error
	if o.dir, err = filepath.Abs(o.dir); err != nil {
		return fail(err)
	}
	if o.m, err = loadManifest(o.dir); err != nil {
		return fail(err)
	}
	for _, w := range luaWarnings(o.dir) {
		fmt.Fprintln(os.Stderr, w)
	}
	if o.self, err = os.Executable(); err != nil {
		return fail(err)
	}
	if o.rexBin, err = exec.LookPath("rex"); err != nil {
		return fail(errors.New("no rex CLI on PATH"))
	}
	if o.lazyrex, err = findLazyRex(o.lazyrex, o.dir); err != nil {
		return fail(err)
	}
	if o.m.Name != "lazyrex" {
		if o.home, err = homePlugin(o.lazyrex); err != nil {
			return fail(err)
		}
	}
	if err := o.m.checkPluginGraph(o.home); err != nil {
		return fail(err)
	}
	files, _ := filepath.Glob(filepath.Join(o.dir, "tests", "*.lua"))
	sort.Strings(files)
	var tests []string
	for _, f := range files {
		if filepath.Base(f) != filepath.Base(o.m.Test.ParityFixture) && (o.file == "" || filepath.Base(f) == o.file) {
			tests = append(tests, f)
		}
	}
	if len(tests) == 0 {
		return fail(fmt.Errorf("no tests/*.lua in %s", o.dir))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var results []*fileResult
	parity := o.runFile(ctx, filepath.Join(o.lazyrex, "lab", "test", "parity.lua"), true)
	parity.name = "parity sweep"
	results = append(results, parity)
	for _, f := range tests {
		if ctx.Err() != nil {
			break
		}
		results = append(results, o.runFile(ctx, f, false))
	}
	code := 0
	total := 0
	for _, r := range results {
		total += r.passed + r.failed
		status := "ok  "
		if !r.ok() {
			status, code = "FAIL", 1
		}
		fmt.Printf("%s %s: %d passed, %d failed\n", status, r.name, r.passed, r.failed)
		for _, p := range r.problems {
			fmt.Printf("     %s\n", p)
		}
		if o.keep && r.labDir != "" {
			fmt.Printf("     lab kept: %s\n", r.labDir)
		}
	}
	if ctx.Err() != nil {
		fmt.Println("FAIL interrupted")
		return 1
	}
	if o.run != "" && total == parity.passed+parity.failed {
		fmt.Printf("FAIL -run %q matched no test\n", o.run)
		return 1
	}
	return code
}

func findLazyRex(flagValue, pluginDir string) (string, error) {
	candidates := []string{flagValue, os.Getenv("LAZYREX_DIR"), filepath.Join(pluginDir, "..", "LazyRex"), pluginDir}
	for d := filepath.Dir(pluginDir); d != "/" && d != "."; d = filepath.Dir(d) {
		candidates = append(candidates, d) // a fixture plugin inside the LazyRex checkout
	}
	if self, err := os.Executable(); err == nil {
		// A binary built in its checkout (hub/lazyrex-hub or hub/cmd/hub/...): walk up.
		for d := filepath.Dir(self); d != "/" && d != "."; d = filepath.Dir(d) {
			candidates = append(candidates, d)
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(c, "lab", "test", "runner.lua")); err == nil {
			return filepath.Abs(c)
		}
	}
	return "", errors.New("cannot find the LazyRex checkout (lab/test/runner.lua): pass -lazyrex or set LAZYREX_DIR")
}

// lab is one isolated Rex server and the processes that the harness started on it.
type lab struct {
	dir, socket, server string
	env                 []string
	srv, hub            *exec.Cmd
	runner              *exec.Cmd // the current runner: a snapshot root while it lives
	srvW, hubW          *waiter
	srvExit, hubExit    chan error
	r                   *rex.Rex
	tracked             map[int]string               // pid -> start time: every process seen under the server or the hub
	args                map[string]map[string]string // plugin args after expansion (manifest test.args)
	watchRoots          map[string][]string          // plugin watch roots after expansion (manifest test.watch_roots)
	profiles            string                       // LAB/profiles.json, the hub's profile file
	restarts            int                          // hub:restart() calls (restartHub)
}

// home is the lab HOME, the working directory of every lab process.
func (l *lab) home() string { return filepath.Join(l.dir, "home") }

func newNonce() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// startLab starts a Rex server in a new directory under /tmp (a Unix socket path must stay short).
func (o *testOpts) startLab(ctx context.Context) (*lab, error) {
	dir, err := os.MkdirTemp("/tmp", "lrt-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	l := &lab{dir: dir, socket: filepath.Join(dir, "rex.sock"), tracked: map[int]string{}}
	l.server = "unix://" + l.socket
	for _, d := range []string{"state", "blocks"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return l, err
		}
	}
	for _, d := range labHomeDirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return l, err
		}
	}
	// The lab environment for every child (isolation.go): no inherited variables except PATH.
	extra, err := o.m.labValues(o.dir, dir, o.fileArgs)
	if err != nil {
		return l, err
	}
	l.env = labEnv(dir, l.server, extra.env)
	l.args, l.watchRoots, l.profiles = extra.args, extra.watchRoots, filepath.Join(dir, "profiles.json")
	for _, roots := range l.watchRoots {
		for _, r := range roots {
			if err := os.MkdirAll(r, 0o700); err != nil {
				return l, err
			}
		}
	}
	if o.m.Test.Profiles != "" {
		b, err := os.ReadFile(filepath.Join(o.dir, o.m.Test.Profiles))
		if err != nil {
			return l, fmt.Errorf("test.profiles: %w", err)
		}
		if err := os.WriteFile(l.profiles, b, 0o600); err != nil {
			return l, err
		}
	}
	logf, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		return l, err
	}
	l.srv = exec.Command(o.rexBin, "-v", "server", "run", "--listen", l.server, "--state-dir", filepath.Join(dir, "state"),
		"--block-dir", filepath.Join(dir, "blocks"), "--config", "/dev/null")
	l.srv.Env, l.srv.Dir, l.srv.Stdout, l.srv.Stderr = l.env, l.home(), logf, logf
	l.srv.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := l.srv.Start(); err != nil {
		return l, err
	}
	l.track(l.srv.Process.Pid)
	l.srvW = watch(l.srv)
	l.srvExit = l.srvW.done
	proof, _ := json.Marshal(map[string]any{"socket": l.server, "server_pid": l.srv.Process.Pid, "nonce": newNonce()})
	if err := os.WriteFile(filepath.Join(dir, "lab.json"), proof, 0o600); err != nil {
		return l, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(l.socket); err == nil {
			dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			r, err := rex.Dial(dctx, l.socket)
			cancel()
			if err == nil {
				l.r = r
				return l, nil
			}
		}
		select {
		case err := <-l.srvExit:
			l.srvExit = nil
			return l, fmt.Errorf("lab server exited: %v (see %s)", err, filepath.Join(dir, "server.log"))
		case <-ctx.Done():
			return l, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return l, errors.New("lab server: not ready after 10 s")
		}
	}
}

// waitRunner reads the runner's result lines until the runner exits, or until fctx ends. Every
// wait after fctx ends has a limit. A hub restart (hub:restart()) runs in the parser with fctx:
// a timeout or an interrupt stops its start of the new hub. waitRunner returns only after the
// parser ends, so the lab cleanup never runs while a restart uses the lab or res.
func (o *testOpts) waitRunner(ctx, fctx context.Context, l *lab, cmd *exec.Cmd, stdout io.ReadCloser, res *fileResult, logf io.Writer) {
	l.track(cmd.Process.Pid)
	l.runner = cmd
	w := watch(cmd)
	restart := o.restartHub
	if o.restartFn != nil {
		restart = o.restartFn
	}
	// mu serializes a restart and the timeout snapshot: both read and write l.hub and l.tracked.
	var mu sync.Mutex
	parsed := make(chan struct{})
	exit := make(chan error, 1)
	go func() {
		o.parse(stdout, res, logf, func(mode string) error {
			mu.Lock()
			defer mu.Unlock()
			return restart(fctx, l, mode)
		})
		close(parsed)
		stdout.Close()
		exit <- <-w.done
	}()
	select {
	case err := <-exit:
		if err != nil {
			res.problems = append(res.problems, "runner exited: "+err.Error())
		}
	case <-fctx.Done():
		// The runner's children, so that the lab cleanup checks them. A restart that runs now ends
		// soon (it stops at fctx; the stop of the old hub has a limit), and the runner waits for it.
		mu.Lock()
		l.snapshot()
		mu.Unlock()
		w.signal(syscall.SIGKILL)
		// Close the output before the wait: another process can keep it open (for example a
		// child after the runner exited; that group gets no signal). The file fails anyway, so
		// lines after the timeout do not count.
		stdout.Close()
		// The parser ends at the closed output, or after a restart that it runs now. A restart
		// has limits after fctx ends: the stop of the old hub (10 s, then SIGKILL) and the
		// start of the new hub (it stops at fctx).
		<-parsed
		select {
		case <-exit:
		case <-time.After(10 * time.Second):
			res.problems = append(res.problems, "runner: not reaped 10 s after the timeout")
		}
		if ctx.Err() != nil {
			res.problems = append(res.problems, "interrupted")
		} else {
			res.problems = append(res.problems, fmt.Sprintf("timeout: the file did not finish in %s", o.timeout))
		}
	}
}

// waiter reaps one started process. It reaps only while it holds mu, and signal holds mu too. So
// a process that signal finds unreaped still owns its pid, and its process group ID (the same
// number) cannot belong to another process. The harness never calls exec.Cmd.Wait for it.
type waiter struct {
	pid    int
	mu     sync.Mutex
	reaped bool
	done   chan error // the exit status, sent once
}

func watch(c *exec.Cmd) *waiter {
	w := &waiter{pid: c.Process.Pid, done: make(chan error, 1)}
	go func() {
		for {
			w.mu.Lock()
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(w.pid, &ws, syscall.WNOHANG, nil)
			if err == syscall.EINTR {
				w.mu.Unlock()
				continue
			}
			if pid == w.pid || err != nil {
				w.reaped = true
				w.mu.Unlock()
				switch {
				case err != nil:
					w.done <- err
				case ws.Signaled():
					w.done <- fmt.Errorf("signal: %v", ws.Signal())
				case ws.ExitStatus() != 0:
					w.done <- fmt.Errorf("exit status %d", ws.ExitStatus())
				default:
					w.done <- nil
				}
				return
			}
			w.mu.Unlock()
			time.Sleep(25 * time.Millisecond)
		}
	}()
	return w
}

// signal sends sig to the process group of the process. It sends nothing and returns false when
// the process is already reaped: its group ID can then name an unrelated group.
func (w *waiter) signal(sig syscall.Signal) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reaped {
		return false
	}
	syscall.Kill(-w.pid, sig)
	return true
}

func (l *lab) track(pid int) {
	if s := startTime(pid); s != "" {
		l.tracked[pid] = s
	}
}

// snapshot records every live descendant of the server and the hub (lab panes are the server's
// children, in their own process groups: observed on Rex 0.0.1792158932-g88fd41, lab).
func (l *lab) snapshot() {
	out, err := exec.Command("ps", "-eo", "pid=,ppid=").Output() // -eo: the same list with BSD and procps ps
	if err != nil {
		return
	}
	children := map[int][]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		children[ppid] = append(children[ppid], pid)
	}
	// A root is a process that the harness started, in the lifetime that it recorded at the start.
	// A pid that now belongs to another process is not a root. A descendant is claimed only when
	// its parent, checked now, is a claimed process: a pid in the old ps listing can be reused.
	type proc struct {
		pid   int
		start string
	}
	var roots []proc
	for _, c := range []*exec.Cmd{l.srv, l.hub, l.runner} {
		if c == nil || c.Process == nil {
			continue
		}
		pid := c.Process.Pid
		if s, ok := l.tracked[pid]; ok && s != "" && startTime(pid) == s {
			roots = append(roots, proc{pid, s})
		}
	}
	for len(roots) > 0 {
		p := roots[0]
		roots = roots[1:]
		for _, c := range children[p.pid] {
			ppid, s, ok := procInfo(c)
			if !ok || ppid != p.pid || startTime(p.pid) != p.start {
				continue // gone, or not a child of the claimed process any more
			}
			l.tracked[c] = s
			roots = append(roots, proc{c, s})
		}
	}
}

// stop ends the hub, then the server, and checks that every tracked process is gone. A process
// that survives makes the cleanup fail; it gets no signal.
func (l *lab) stop() []string {
	var problems []string
	l.snapshot()
	if l.r != nil {
		l.r.Close()
	}
	// A leader that already exited gets no group signal (waiter.signal). Its group members are in
	// tracked; a member that survives fails the cleanup below.
	stopGroup := func(w *waiter, exit chan error, what string) {
		if w == nil || exit == nil {
			return
		}
		w.signal(syscall.SIGTERM)
		select {
		case <-exit:
		case <-time.After(10 * time.Second):
			problems = append(problems, what+" did not stop in 10 s after SIGTERM; sent SIGKILL")
			w.signal(syscall.SIGKILL)
			<-exit
		}
	}
	stopGroup(l.hubW, l.hubExit, "hub")
	l.snapshot()
	stopGroup(l.srvW, l.srvExit, "lab server")
	deadline := time.Now().Add(5 * time.Second)
	for {
		var alive []int
		for pid, start := range l.tracked {
			if startTime(pid) == start {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 {
			break
		}
		if time.Now().After(deadline) {
			// The harness sends a survivor no signal: between a lifetime check and kill() its pid
			// can be reused, and macOS has no process handle to close that gap. A survivor is a
			// harness bug. The run fails and names it.
			sort.Ints(alive)
			for _, pid := range alive {
				argv, _ := procArgv(pid)
				problems = append(problems, fmt.Sprintf("cleanup: process survived the lab server: pid %d, start %s, command %s (not signaled)",
					pid, l.tracked[pid], strings.Join(argv, " ")))
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return problems
}

// promptGrant names the plugins with the prompts grant: test.files prompts of the file that runs
// now, else test.prompts.
func (o *testOpts) promptGrant() []string {
	if o.filePrompts.set {
		return o.filePrompts.names
	}
	return o.m.Test.Prompts
}

func (o *testOpts) hubConfig(l *lab) (string, error) {
	// Local source entries only (docs/plugin-sources.md, section 9): the lab needs no lock. The hub
	// reads each manifest itself.
	type plug struct {
		Source     string            `json:"source"`
		Args       map[string]string `json:"args,omitempty"`
		WatchRoots []string          `json:"watch_roots,omitempty"`
		Prompts    bool              `json:"prompts,omitempty"`
	}
	plugins := []plug{}
	add := func(dir, name string) {
		plugins = append(plugins, plug{Source: dir, Args: l.args[name], WatchRoots: l.watchRoots[name], Prompts: contains(o.promptGrant(), name)})
	}
	if o.home != nil { // the home plugin: every real hub config loads it
		add(o.home.dir, "lazyrex")
	}
	for _, d := range o.m.deps {
		add(d.dir, d.m.Name)
	}
	add(o.dir, o.m.Name)
	agents := []manifestAgent{{ID: "user", Name: "User"}}
	agents = append(agents, o.m.Test.Agents...)
	view := filepath.Join(l.dir, "testview.sh")
	script := "#!/bin/sh\nexec " + shellQuote(o.self) + " testview -hold " + shellQuote(filepath.Join(l.dir, "testview-hold")) + " \"$@\"\n"
	if err := os.WriteFile(view, []byte(script), 0o700); err != nil {
		return "", err
	}
	cfg := map[string]any{"server": l.server, "state": filepath.Join(l.dir, "hub"), "rex": o.rexBin, "view": view,
		"agents": agents, "plugins": plugins, "transcript": o.m.Test.Transcript,
		"profiles": l.profiles, "test_roots": o.m.roots(o.dir, l.dir)}
	b, _ := json.MarshalIndent(cfg, "", " ")
	path := filepath.Join(l.dir, "hub.json")
	return path, os.WriteFile(path, b, 0o600)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// startHub starts this binary as the hub and waits until every plugin is registered and the agent
// face lists the plugin's forwarded exports.
func (o *testOpts) startHub(ctx context.Context, l *lab) error {
	cfg, err := o.hubConfig(l)
	if err != nil {
		return err
	}
	logf, err := os.Create(filepath.Join(l.dir, "hub.log"))
	if err != nil {
		return err
	}
	l.hub = exec.Command(o.self, "-config", cfg, "-test", "-sdk", filepath.Join(o.lazyrex, "sdk", "lua"))
	l.hub.Env, l.hub.Dir, l.hub.Stdout, l.hub.Stderr = l.env, l.home(), logf, logf
	l.hub.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := l.hub.Start(); err != nil {
		return err
	}
	l.track(l.hub.Process.Pid)
	l.hubW = watch(l.hub)
	l.hubExit = l.hubW.done
	c := &helperConn{r: l.r}
	deadline := time.Now().Add(20 * time.Second)
	var last string
	var registeredAt time.Time
	for {
		select {
		case err := <-l.hubExit:
			l.hubExit = nil
			return fmt.Errorf("hub exited: %v (see %s)", err, filepath.Join(l.dir, "hub.log"))
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("hub not ready after 20 s: %s (see %s)", last, filepath.Join(l.dir, "hub.log"))
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		hub, err := c.resolveClient(cctx, "lazyrex-hub")
		var raw json.RawMessage
		if err == nil {
			raw, err = c.perform(cctx, hub, "hub.test.status", nil)
		}
		cancel()
		if err != nil {
			last = err.Error()
			continue
		}
		var st struct {
			Plugins map[string]struct {
				Registered bool     `json:"registered"`
				State      string   `json:"state"`
				Error      string   `json:"error"`
				Waiting    []string `json:"waiting"`
				Exports    []string `json:"exports"`
				Face       []string `json:"face"`
			} `json:"plugins"`
		}
		json.Unmarshal(raw, &st)
		ready := true
		for name, p := range st.Plugins {
			if p.State == "refused" || p.State == "retained" {
				return fmt.Errorf("plugin %s is %s: %s (see %s)", name, p.State, p.Error, filepath.Join(l.dir, "hub.log"))
			}
			if p.State == "waiting" {
				ready, last = false, "plugin "+name+" waits for "+strings.Join(p.Waiting, ", ")+" to register"
			} else if !p.Registered {
				ready, last = false, "plugin "+name+" is not registered"
			}
		}
		p := st.Plugins[o.m.Name]
		if ready && len(p.Face) < len(p.Exports) {
			// The agent face declares exports after registration (agentKeymapLoop). A plugin that
			// does not declare hub.forward never gets them: parity reports that, not readiness.
			if registeredAt.IsZero() {
				registeredAt = time.Now()
			}
			if time.Since(registeredAt) < 3*time.Second {
				ready, last = false, "the agent face does not list the exports yet"
			}
		}
		if ready {
			return nil
		}
	}
}

// runFile runs one test file in its own lab and hub.
func (o *testOpts) runFile(ctx context.Context, file string, parity bool) *fileResult {
	res := &fileResult{name: strings.TrimPrefix(file, o.dir+"/"), parity: parity}
	o.fileArgs, o.filePrompts = nil, filePrompts{}
	if !parity {
		f := o.m.Test.Files[filepath.Base(file)]
		o.fileArgs = f.Args
		o.filePrompts = f.Prompts
	}
	l, err := o.startLab(ctx)
	if l != nil {
		res.labDir = l.dir
		defer func() {
			res.problems = append(res.problems, l.stop()...)
			if !o.keep {
				os.RemoveAll(l.dir)
			}
		}()
	}
	if err != nil {
		res.problems = append(res.problems, "lab: "+err.Error())
		return res
	}
	if err := o.startHub(ctx, l); err != nil {
		res.problems = append(res.problems, err.Error())
		return res
	}
	var sess struct {
		SessionID string `json:"session_id"`
	}
	if err := l.r.Call(ctx, "session.create", map[string]any{"label": "tests", "initial_windows": []any{map[string]any{"window_label": "runner",
		"layout": map[string]any{"block": map[string]any{"flavor": "com.superlogical.terminal.shell", "options": map[string]any{"command": []string{"/bin/sleep", "100000"}}}}}}}, &sess); err != nil {
		res.problems = append(res.problems, "tests session: "+err.Error())
		return res
	}
	args := []string{"-S", l.server, "--autostart=false", "do", filepath.Join(o.lazyrex, "lab", "test", "runner.lua"), "-s", sess.SessionID,
		"file=" + file, "lib=" + filepath.Join(o.lazyrex, "lab", "test"), "sdk=" + filepath.Join(o.lazyrex, "sdk", "lua"), "hub_bin=" + o.self, "plugin=" + o.m.Name, "tests_session=" + sess.SessionID,
		"lab=" + l.dir, "profiles=" + l.profiles}
	if o.run != "" && !parity {
		args = append(args, "run="+o.run)
	}
	if parity {
		routes, _ := json.Marshal(o.m.Test.ParityRoutes)
		args = append(args, "parity_routes="+string(routes))
		if o.m.Test.ParityFixture != "" {
			args = append(args, "fixture="+filepath.Join(o.dir, o.m.Test.ParityFixture))
		}
	}
	fctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	cmd := exec.Command(o.rexBin, args...)
	cmd.Env, cmd.Dir = l.env, l.home()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, _ := cmd.StdoutPipe()
	logf, _ := os.Create(filepath.Join(l.dir, "runner.log"))
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		res.problems = append(res.problems, "runner: "+err.Error())
		return res
	}
	o.waitRunner(ctx, fctx, l, cmd, stdout, res, logf)
	logf.Close()
	if !res.ok() || o.verbose {
		for _, name := range []string{"runner.log", "hub.log"} {
			tail(filepath.Join(l.dir, name), res, o.verbose)
		}
	}
	return res
}

var lrtLine = regexp.MustCompile(`^LRT (plan (\d+)|ok (\d+) (.*)|fail (\d+) (.*)|done (\d+) (\d+)|note (.*)|restart (term|kill))$`)

// parse reads LRT lines. The file fails without a plan, without a done line, with counts that do
// not match the plan, with a malformed LRT line, or with zero tests.
//
// "LRT restart term|kill" (hub:restart() in the runner) restarts the hub now; the runner waits for
// the new hub itself. A failed restart is a problem of the file.
func (o *testOpts) parse(r io.Reader, res *fileResult, logw io.Writer, restart func(mode string) error) {
	plan, seen := -1, 0
	done := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		fmt.Fprintln(logw, line)
		if !strings.HasPrefix(line, "LRT ") {
			if o.verbose {
				fmt.Println("  |", line)
			}
			continue
		}
		m := lrtLine.FindStringSubmatch(line)
		switch {
		case m == nil:
			res.problems = append(res.problems, "malformed result line: "+line)
		case done:
			res.problems = append(res.problems, "output after the done line: "+line)
		case m[2] != "":
			if plan >= 0 {
				res.problems = append(res.problems, "a second plan line")
			}
			plan, _ = strconv.Atoi(m[2])
		case m[3] != "":
			if i, _ := strconv.Atoi(m[3]); i != seen+1 {
				res.problems = append(res.problems, fmt.Sprintf("result %d out of order (expected %d): %s", i, seen+1, line))
			}
			seen++
			res.passed++
			if o.verbose {
				fmt.Printf("  ok %s\n", m[4])
			}
		case m[5] != "":
			if i, _ := strconv.Atoi(m[5]); i != seen+1 {
				res.problems = append(res.problems, fmt.Sprintf("result %d out of order (expected %d): %s", i, seen+1, line))
			}
			seen++
			res.failed++
			fmt.Printf("  FAIL %s\n", unescapeLRT(m[6]))
		case m[7] != "":
			done = true
			p, _ := strconv.Atoi(m[7])
			f, _ := strconv.Atoi(m[8])
			if p != res.passed || f != res.failed {
				res.problems = append(res.problems, fmt.Sprintf("the done line says %d passed and %d failed; the harness counted %d and %d", p, f, res.passed, res.failed))
			}
		case m[9] != "":
			fmt.Printf("  note %s\n", unescapeLRT(m[9]))
		case m[10] != "":
			if restart == nil {
				res.problems = append(res.problems, "a hub restart is not available here")
			} else if err := restart(m[10]); err != nil {
				res.problems = append(res.problems, "hub restart: "+err.Error())
			}
		}
	}
	if err := sc.Err(); err != nil {
		res.problems = append(res.problems, "reading the runner output: "+err.Error())
	}
	switch {
	case plan < 0:
		res.problems = append(res.problems, "no plan line: the file stopped before its tests ran")
	case plan == 0 && o.run != "" && !res.parity:
		// -run selects tests across files; testMain fails the run if no file ran a test.
	case plan == 0:
		res.problems = append(res.problems, "zero tests (an empty file, or -run matched nothing)")
	case seen != plan:
		res.problems = append(res.problems, fmt.Sprintf("the plan names %d tests, %d reported", plan, seen))
	}
	if !done && plan >= 0 {
		res.problems = append(res.problems, "no done line: the runner stopped early")
	}
}

// unescapeLRT reverses the runner's escaping of new lines in messages.
func unescapeLRT(s string) string {
	return strings.NewReplacer(`\n`, "\n       ", `\\`, `\`).Replace(s)
}

func tail(path string, res *fileResult, all bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if !all && len(lines) > 25 {
		lines = lines[len(lines)-25:]
	}
	fmt.Printf("  --- %s (%s)\n", filepath.Base(path), path)
	for _, line := range lines {
		fmt.Println("  |", line)
	}
}

// restartHub stops the lab hub that this command started and starts it again with the same config,
// lab server and state (hub:restart() in a test; the coordinator, 2026-10-19). mode term sends
// SIGTERM to the hub's process group (a stop), kill sends SIGKILL (a crash). Only the hub's own
// group gets a signal, through its waiter: a reaped hub gets none. The old hub's log is kept as
// hub.<n>.log. Processes that the old hub left stay tracked, so the lab cleanup still checks them.
func (o *testOpts) restartHub(ctx context.Context, l *lab, mode string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.hubW == nil || l.hubExit == nil {
		return fmt.Errorf("the hub is not running")
	}
	sig := syscall.SIGTERM
	if mode == "kill" {
		sig = syscall.SIGKILL
	}
	l.snapshot()
	l.hubW.signal(sig)
	select {
	case <-l.hubExit:
	case <-time.After(10 * time.Second):
		l.hubW.signal(syscall.SIGKILL)
		<-l.hubExit
		return fmt.Errorf("the hub did not stop in 10 s after %v; sent SIGKILL", sig)
	}
	l.hubExit = nil
	l.restarts++
	if err := os.Rename(filepath.Join(l.dir, "hub.log"), filepath.Join(l.dir, fmt.Sprintf("hub.%d.log", l.restarts))); err != nil {
		return err
	}
	return o.startHub(ctx, l)
}

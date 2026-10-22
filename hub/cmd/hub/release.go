package main

// lazyrex-hub release <step> (docs/releasing.md): the release tool. It moves the live hub, or one
// managed plugin, from one commit to another, and rehearses the same steps in a lab first. One
// step runs per call. Each step exits 1 at the first unexpected state, before any mutation that
// depends on that state.
//
// The same binary runs the hub, so this file and the other release*.go files keep no package
// state: no init and no global that the hub's own run reads or writes. Test hooks are fields of
// the releaser.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Exit statuses of lazyrex-hub release.
const (
	relOK       = 0
	relFailed   = 1
	relUsage    = 2
	relTestStop = 3 // RELEASE_TEST_STOP stopped the step (labs and tests only)
)

// relSpec is the spec file of one release: the only input that changes between releases. Every
// other value (OLD, the pins, the capabilities of OLD) is read at freeze.
type relSpec struct {
	Kind       string   `json:"kind"`        // hub or plugin
	H          string   `json:"h"`           // hub: the target commit of LazyRex
	Plugin     string   `json:"plugin"`      // plugin: the plugin name
	To         string   `json:"to"`          // plugin: the target commit
	Tag        string   `json:"tag"`         // plugin: the tag for the target, vX.Y.Z (optional)
	Config     string   `json:"config"`      // the hub config
	ReleaseDir string   `json:"release_dir"` // hub: the directory of the release clones
	Server     string   `json:"server"`      // the Rex server of the hub
	DevLog     string   `json:"dev_log"`     // hub: the log of the dev pane
	Real       bool     `json:"real"`        // the live hub: DEV_REAL=1 for dev.sh
	Lab        bool     `json:"lab"`         // a lab run (rehearse): every path in the lab, a proved lab server
	Probes     string   `json:"probes"`      // optional SQL probe file for the tables that H adds
	Absent     []string `json:"absent"`      // paths that must not exist at backup
	// LabArgs maps a plugin arg that names a path to its value in the lab (rehearse).
	LabArgs map[string]map[string]string `json:"lab_args"`
}

var (
	relCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
	relTag    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
)

// readSpec reads and checks a spec file. Unknown fields are refused.
func readSpec(path string) (*relSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s relSpec
	if err := decodeStrict(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := s.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

func (s *relSpec) check() error {
	if s.Lab && s.Real {
		return errors.New(`a spec cannot be "lab" and "real"`)
	}
	need := func(name, v string) error {
		if v == "" {
			return fmt.Errorf("%s is required for a %s release", name, s.Kind)
		}
		return nil
	}
	for _, f := range []struct{ name, v string }{{"config", s.Config}, {"server", s.Server}} {
		if err := need(f.name, f.v); err != nil {
			return err
		}
	}
	switch s.Kind {
	case "hub":
		if !relCommit.MatchString(s.H) {
			return errors.New("h must be a commit of 40 lowercase hex")
		}
		for _, f := range []struct{ name, v string }{{"release_dir", s.ReleaseDir}, {"dev_log", s.DevLog}} {
			if err := need(f.name, f.v); err != nil {
				return err
			}
		}
		if s.Plugin != "" || s.To != "" || s.Tag != "" {
			return errors.New("plugin, to and tag are for a plugin release")
		}
	case "plugin":
		if !pluginName.MatchString(s.Plugin) {
			return errors.New("plugin must be a plugin name")
		}
		if !relCommit.MatchString(s.To) {
			return errors.New("to must be a commit of 40 lowercase hex")
		}
		if s.Tag != "" && !relTag.MatchString(s.Tag) {
			return errors.New("tag must have the form vX.Y.Z")
		}
		if s.H != "" || s.ReleaseDir != "" || s.DevLog != "" || s.Probes != "" {
			return errors.New("h, release_dir, dev_log and probes are for a hub release")
		}
	default:
		return errors.New(`kind must be "hub" or "plugin"`)
	}
	for _, p := range append([]string{s.Config, s.ReleaseDir, s.DevLog, s.Probes}, s.Absent...) {
		if p != "" && !filepath.IsAbs(p) {
			return fmt.Errorf("%s is not an absolute path", p)
		}
	}
	return nil
}

// relCmd is one external command: git, go, rex, or a hub binary.
type relCmd struct {
	Dir  string
	Env  []string // nil: the releaser's child environment
	Argv []string
}

// releaser runs the steps of one run directory.
type releaser struct {
	run    string // the run directory
	spec   *relSpec
	step   string
	out    io.Writer
	cfg    *pluginCmdConfig // the config, lock and managed file paths of spec.Config
	ctx    context.Context
	env    []string // the child environment (childEnv)
	unlock func()   // releases the operator flock

	// Hooks for the unit tests. Nil: the real implementation.
	exec       func(c relCmd) (stdout, stderr []byte, err error)
	buildInfo  func(path string) (revision string, modified bool, err error)
	now        func() time.Time
	alive      func(pid int, start string) bool // a process lifetime still runs
	startOf    func(pid int) string             // the start time of a process, "" if gone
	exeOf      func(pid int) string             // the executable of a process
	sleep      func(d time.Duration)
	tool       string                              // the tool binary for the rehearsal gate; "": this executable
	syncDigest func(source string) (string, error) // the digest of a commit-form source
	// syncManifest is the digest and the manifest of a commit-form source (a plugin freeze).
	syncManifest func(source string) (string, []byte, error)
	buildRecord  func(path string) ([]string, error) // the build record of a Go binary
	argvOf       func(pid int) ([]string, error)     // the argv of a process
	// testStop allows RELEASE_TEST_STOP outside a proved lab (unit tests only).
	testStop string
	// rehearseLab and rehearseTier are the -lab and -tier flags of rehearse.
	rehearseLab, rehearseTier string
}

func (r *releaser) say(format string, args ...any) {
	fmt.Fprintf(r.out, "release %s: %s\n", r.step, fmt.Sprintf(format, args...))
}

// relError is a step failure: the text says what state was unexpected.
type relError struct{ msg string }

func (e *relError) Error() string { return e.msg }

func relFail(format string, args ...any) error { return &relError{msg: fmt.Sprintf(format, args...)} }

// errTestStop is the gated stop of RELEASE_TEST_STOP.
var errTestStop = errors.New("stopped by RELEASE_TEST_STOP")

// childEnv is the environment of every child: the caller's, without the Rex selectors of the
// caller's pane, and with REX_SERVER set to the release's server.
func childEnv(base []string, server string) []string {
	var env []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "REX_SESSION", "REX_BLOCK", "REX_CLIENT", "REX_SERVER":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "REX_SERVER="+server)
}

// runCmd runs an external command. A nonzero exit is an error that carries stderr.
func (r *releaser) runCmd(c relCmd) (stdout, stderr []byte, err error) {
	if c.Env == nil {
		c.Env = r.env
	}
	if r.exec != nil {
		return r.exec(c)
	}
	var o, e bytes.Buffer
	cmd := exec.CommandContext(r.ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = c.Dir, c.Env, &o, &e
	err = cmd.Run()
	return o.Bytes(), e.Bytes(), err
}

// cmdError is the text of a failed command: its exit and its stderr.
func cmdError(argv []string, stderr []byte, err error) error {
	return relFail("%s: %v: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(stderr)))
}

// rex runs the rex CLI on the release's server, never with autostart.
func (r *releaser) rex(args ...string) ([]byte, error) {
	argv := append([]string{"rex", "-S", r.spec.Server, "--autostart=false", "--timeout", "30s"}, args...)
	o, e, err := r.runCmd(relCmd{Argv: argv})
	if err != nil {
		return nil, cmdError(argv, e, err)
	}
	return o, nil
}

// hubDo calls a hub action on the hub client.
func (r *releaser) hubDo(client, action string, args any) ([]byte, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	return r.rex("-C", client, "do", action, "--args", string(b))
}

// The operator lock: one release step at a time for a config, across run directories.
func operatorLockPath(cfgPath string) string {
	return strings.TrimSuffix(cfgPath, ".json") + ".release.lock"
}

// lockOperator takes the operator flock without waiting. The holder's pid is in the file, for the
// refusal text of a second operator.
func lockOperator(cfgPath string) (func(), error) {
	p := operatorLockPath(cfgPath)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		b, _ := os.ReadFile(p)
		f.Close()
		return nil, relFail("another release step runs (pid %s; %s)", strings.TrimSpace(string(b)), p)
	}
	f.Truncate(0)
	f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	return func() { f.Close() }, nil
}

// The journal: <run>/journal.jsonl, one record per line, each line synced before the next action.
// It is evidence and holds the saved expectations of mutations. A decision on a rerun comes from
// the real state; the journal alone never proves that a mutation happened.
type relRecord struct {
	At    string          `json:"at"`
	Step  string          `json:"step"`
	Op    string          `json:"op"`
	Phase string          `json:"phase"` // begin, sent, failed or end
	Data  json.RawMessage `json:"data,omitempty"`
}

func (r *releaser) journalPath() string { return filepath.Join(r.run, "journal.jsonl") }

func (r *releaser) appendJournal(op, phase string, data any) error {
	rec := relRecord{At: r.now().UTC().Format(time.RFC3339Nano), Step: r.step, Op: op, Phase: phase}
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		rec.Data = b
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(r.journalPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// journal reads every record.
func (r *releaser) journal() ([]relRecord, error) {
	b, err := os.ReadFile(r.journalPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []relRecord
	for i, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec relRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", r.journalPath(), i+1, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// lastBegin returns the data of the last begin record of op that has no end record after it: the
// saved expectation of an interrupted mutation. ok is false when every begin has its end. A failed
// record does not end a mutation: its outcome can be unknown (a lost reply after a retirement), so
// a rerun still needs the expectation of the begin record before it.
func (r *releaser) lastBegin(op string) (data json.RawMessage, ok bool, err error) {
	recs, err := r.journal()
	if err != nil {
		return nil, false, err
	}
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Op != op {
			continue
		}
		switch recs[i].Phase {
		case "end":
			return nil, false, nil
		case "begin":
			return recs[i].Data, true, nil
		}
	}
	return nil, false, nil
}

// sentAfterBegin reports whether op has a "sent" record after its last begin record: the request of
// the open mutation went out, and its result is unknown.
func (r *releaser) sentAfterBegin(op string) (bool, error) {
	recs, err := r.journal()
	if err != nil {
		return false, err
	}
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Op != op {
			continue
		}
		switch recs[i].Phase {
		case "sent":
			return true, nil
		case "begin", "end":
			return false, nil
		}
	}
	return false, nil
}

// mutate runs one mutation between its begin and end records. RELEASE_TEST_STOP=<op>:begin stops
// after the begin record, before the mutation; <op>:after stops after the mutation, before the end
// record. Both leave the state of an interrupted step.
func (r *releaser) mutate(op string, expect any, fn func() (any, error)) error {
	if err := r.appendJournal(op, "begin", expect); err != nil {
		return err
	}
	if err := r.maybeStop(op, "begin"); err != nil {
		return err
	}
	res, err := fn()
	if err != nil {
		r.appendJournal(op, "failed", map[string]string{"error": err.Error()})
		return err
	}
	if err := r.maybeStop(op, "after"); err != nil {
		return err
	}
	return r.appendJournal(op, "end", res)
}

// maybeStop honors RELEASE_TEST_STOP only in a proved lab (checkLabProof of the spec's server, and
// a spec without real), or with the unit-test hook. Elsewhere a set variable fails the step: a
// live run never stops half way on purpose.
func (r *releaser) maybeStop(op, phase string) error {
	v := r.testStop
	if v == "" {
		v = os.Getenv("RELEASE_TEST_STOP")
		if v == "" {
			return nil
		}
		if r.spec.Real {
			return relFail("RELEASE_TEST_STOP is set on a real release; unset it")
		}
		if err := checkLabProof(r.spec.Server); err != nil {
			return relFail("RELEASE_TEST_STOP needs a proved lab server: %v", err)
		}
	}
	if v == op+":"+phase {
		r.say("RELEASE_TEST_STOP at %s:%s", op, phase)
		return errTestStop
	}
	return nil
}

// releaseMain runs lazyrex-hub release <step> -run <dir> [flags].
func releaseMain(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "usage: lazyrex-hub release <step> -run <dir> (docs/releasing.md)")
		return relUsage
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage()
	}
	step, ok := relStepFor(args[0])
	if !ok {
		fmt.Fprintf(stderr, "release: unknown step %q\n", args[0])
		return usage()
	}
	r, code := step.open(args[0], args[1:], stdout, stderr)
	if r == nil {
		return code
	}
	defer r.unlock()
	err := step.run(r)
	switch {
	case err == nil:
		return relOK
	case errors.Is(err, errTestStop):
		return relTestStop
	default:
		fmt.Fprintf(stderr, "release %s: FAILED: %v\n", r.step, err)
		return relFailed
	}
}

// createRun makes a new run directory (mode 0700; it must not exist) with a copy of the spec file
// as spec.json. Freeze calls it.
func createRun(dir, specPath string) error {
	b, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	var s relSpec
	if err := decodeStrict(b, &s); err != nil {
		return fmt.Errorf("%s: %w", specPath, err)
	}
	if err := s.check(); err != nil {
		return fmt.Errorf("%s: %w", specPath, err)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("the run directory %s is not an absolute path", dir)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("the run directory: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, relSpecFile), b, 0o600)
}

// openRun opens the run directory dir for one step: it reads spec.json and the config paths, and
// takes the operator flock. The caller calls r.unlock.
func openRun(dir, step string, stdout io.Writer) (*releaser, error) {
	s, err := readSpec(filepath.Join(dir, relSpecFile))
	if err != nil {
		return nil, err
	}
	cfg, err := readPluginCmdConfig(s.Config)
	if err != nil {
		return nil, err
	}
	// Before the lock file write and before any child.
	if err := checkLabSpec(dir, s, cfg); err != nil {
		return nil, err
	}
	unlock, err := lockOperator(s.Config)
	if err != nil {
		return nil, err
	}
	return &releaser{run: dir, spec: s, step: step, out: stdout, cfg: cfg, ctx: context.Background(),
		env: childEnv(os.Environ(), s.Server), unlock: unlock, now: time.Now}, nil
}

// openStep is the open of a step that takes only -run.
func openStep(name string, args []string, stdout, stderr io.Writer) (*releaser, int) {
	fs := flag.NewFlagSet("release "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("run", "", "the run directory (required)")
	if err := fs.Parse(args); err != nil || *dir == "" || fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage: lazyrex-hub release %s -run <dir>\n", name)
		return nil, relUsage
	}
	r, err := openRun(*dir, name, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "release %s: FAILED: %v\n", name, err)
		return nil, relFailed
	}
	return r, relOK
}

// relStep is one subcommand of release.
type relStep struct {
	open func(name string, args []string, stdout, stderr io.Writer) (*releaser, int)
	run  func(r *releaser) error
}

// relStepFor returns the step of a subcommand name. The steps are a switch, not a package map:
// release code keeps no package state (see the top of this file).
func relStepFor(name string) (relStep, bool) {
	// step: the hub step and the plugin step (nil: not a step of a plugin release).
	step := func(hub, plugin func(*releaser) error) (relStep, bool) {
		return relStep{open: openStep, run: relKinds(hub, plugin)}, true
	}
	gated := func(hub, plugin func(*releaser) error) (relStep, bool) {
		if plugin != nil {
			plugin = noPendingCreate(plugin)
		}
		return step(noPendingCreate(hub), plugin)
	}
	switch name {
	case "freeze":
		return relStep{open: openFreeze, run: relKinds(hubFreeze, relPluginFreeze)}, true
	case "precheck":
		return gated(hubPrecheck, relPluginPrecheck)
	case "stop":
		return gated(hubStop, nil)
	case "backup":
		return gated(hubBackup, hubBackup)
	case "checkout":
		return gated(hubCheckout, nil)
	case "pull":
		return gated(hubPull, relPluginPull)
	case "start":
		return step(hubStart, nil)
	case "replace":
		return gated(hubReplace, relPluginReplace)
	case "verify":
		return gated(hubVerify, relPluginVerify)
	case "rollback":
		return step(hubRollback, relPluginRollback)
	case "resume":
		return step(hubResume, nil)
	case "rehearse":
		return relStep{open: openRehearse, run: relKinds(hubRehearse, nil)}, true
	}
	return relStep{}, false
}

// checkLabSpec checks the lab of a spec with "lab": the server has a lab proof (checkLabProof), its
// lab dir L is the dir of the socket, the config names the same server, and every path that the
// steps write or run resolves inside L: the config, its state, lock and managed file (after any
// override in the config), view, profiles and palette_lua, the release dir and its dev.sh, the dev
// log, the probe file and the run dir. A spec without "lab" must not name a lab server.
func checkLabSpec(run string, s *relSpec, cfg *pluginCmdConfig) error {
	if !s.Lab {
		if checkLabProof(s.Server) == nil {
			return relFail("the server %s is a lab server; a lab spec must say \"lab\": true", s.Server)
		}
		return nil
	}
	if err := checkLabProof(s.Server); err != nil {
		return relFail("lab spec: %v", err)
	}
	socket, err := rexSocket(s.Server)
	if err != nil {
		return relFail("lab spec: %v", err)
	}
	lab, err := resolveExisting(filepath.Dir(socket))
	if err != nil {
		return relFail("lab spec: %v", err)
	}
	b, err := os.ReadFile(cfg.path)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return relFail("lab spec: the config %s: %v", cfg.path, err)
	}
	if raw["server"] != s.Server {
		return relFail("lab spec: the config server %v is not the spec server %s", raw["server"], s.Server)
	}
	type labPath struct{ key, p string }
	paths := []labPath{{"config", cfg.path}, {"state", cfg.state}, {"lock", cfg.lock}, {"managed file", cfg.managed},
		{"run dir", run}, {"probes", s.Probes}}
	for _, k := range []string{"view", "profiles", "palette_lua"} {
		if v, ok := raw[k].(string); ok {
			paths = append(paths, labPath{"config " + k, v})
		} else if raw[k] != nil {
			return relFail("lab spec: the config %s is not a string", k)
		}
	}
	if s.Kind == "hub" {
		paths = append(paths, labPath{"release_dir", s.ReleaseDir},
			labPath{"dev.sh", filepath.Join(s.ReleaseDir, "LazyRex", "dev.sh")}, labPath{"dev_log", s.DevLog})
	}
	for _, x := range paths {
		if x.p == "" {
			continue
		}
		p, err := filepath.Abs(x.p)
		if err == nil {
			p, err = resolveExisting(p)
		}
		if err != nil {
			return relFail("lab spec: %s: %v", x.key, err)
		}
		if !within(p, lab) {
			return relFail("lab spec: the %s %s is outside the lab %s", x.key, x.p, lab)
		}
	}
	return nil
}

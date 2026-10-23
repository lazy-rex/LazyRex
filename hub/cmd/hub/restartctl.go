package main

// lazyrex-hub stop|status -config <cfg> [-real-server] and lazyrex-hub restart -config <cfg>
// [-real-server] -expect-exec <n> (docs/hub.md, "Stop and status", "Restart"). Each reads the
// config, then looks at two things:
//   - the hub of this config: a connected Rex client labeled lazyrex-hub whose hub.info state is
//     the config's canonical state;
//   - the restart wrapper of that state: the holder of the fcntl lock on <state>/restart.lock,
//     and <state>/restart.json, trusted only when its pid is the holder and that pid has the
//     start time in the file.
// stop never sends a signal: a running hub gets hub.admin.shutdown (the admin guard applies), and
// a wrapper without a hub (starting, backoff) is stopped in its own pane. restart sends
// hub.admin.relaunch once to a hub that runs under its wrapper, then watches the wrapper exec itself.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// Exit statuses of status (stop uses 0, 1, 3 and 4 the same way; 0 also for "not running").
const (
	ctlRunning    = 0
	ctlNotRunning = 1
	ctlUsage      = 2
	ctlWrapper    = 3 // a wrapper owns the state, and no hub of it runs (starting, backoff)
	ctlUnknown    = 4
)

var ctlStopWait = 10 * time.Second // a test shortens it

var ctlRestartWait = 60 * time.Second // a test shortens it

type ctlView struct {
	cfg      Config
	rex      string
	state    string // canonical; "" when the state directory does not exist
	own      *hubClient
	others   []hubClient // hubs of other states on this Rex server
	rexErr   error       // the Rex server could not be asked
	holder   int         // the restart.lock holder; 0 when none
	status   *restartStatus
	trusted  bool     // status belongs to holder
	ownTwice bool     // more than one connected hub reports this state
	localErr error    // the state or its restart lock cannot be read: nothing about this state is known
	plugins  []string // the refused plugins of the own hub (pluginrefusal.go); nil when not read
	agents   []string // running agent panes without a current identity (agentidentity.go); nil when not read
	remote   []string // the last observed Remote Control result of each connected app (remotecontrol.go); nil when not read
}

type hubClient struct {
	ID     string
	State  string
	PID    int
	Exec   int    // the exec count of its restart wrapper (hub.info)
	Commit string // the build's VCS revision (hub.info)
}

func ctlMain(cmd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "hub configuration JSON (required)")
	realServer := fs.Bool("real-server", false, "the config names the user's real Rex server")
	usage := fmt.Sprintf("usage: lazyrex-hub %s -config <cfg> [-real-server]", cmd)
	expectExec := -1
	if cmd == "restart" {
		fs.IntVar(&expectExec, "expect-exec", -1, "the exec count that lazyrex-hub status shows (required)")
		usage += " -expect-exec <n>"
	}
	if err := fs.Parse(args); err != nil || *cfgPath == "" || fs.NArg() > 0 || (cmd == "restart" && expectExec < 0) {
		fmt.Fprintln(stderr, usage)
		return ctlUsage
	}
	cfg, err := loadConfig(*cfgPath)
	if err == nil {
		err = checkServer(context.Background(), cfg.Server, *realServer, false)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ctlUsage
	}
	v := ctlLook(cfg)
	if cmd == "status" {
		v.readPlugins()
		v.readAgents()
		v.readRemoteControl()
		code, text := v.statusText()
		fmt.Fprintln(stdout, text)
		return code
	}
	if cmd == "restart" {
		return v.restart(stdout, expectExec, func() *ctlView { return ctlLook(cfg) })
	}
	return v.stop(stdout)
}

func ctlLook(cfg Config) *ctlView {
	v := &ctlView{cfg: cfg}
	v.rex, v.rexErr = findRex(cfg.Rex)
	if s, err := canonicalState(cfg.State); err == nil {
		v.state = s
	} else if !errors.Is(err, fs.ErrNotExist) {
		v.localErr = fmt.Errorf("the state %s cannot be resolved: %v", cfg.State, err)
	}
	if v.state != "" {
		v.holder, v.status, v.trusted, v.localErr = readRestart(v.state)
	}
	if v.rexErr == nil {
		var hubs []hubClient
		hubs, v.rexErr = v.hubs()
		for i, h := range hubs {
			if v.state != "" && h.State == v.state {
				if v.own != nil {
					v.ownTwice = true
				}
				v.own = &hubs[i]
			} else {
				v.others = append(v.others, h)
			}
		}
	}
	return v
}

// readRestart reads the restart lock holder and the status file. It never creates the lock file
// and never takes a lock: F_GETLK asks only. A lock file that exists but cannot be asked is an error.
func readRestart(state string) (holder int, st *restartStatus, trusted bool, err error) {
	lock := filepath.Join(state, "restart.lock")
	if f, oerr := os.Open(lock); oerr == nil {
		holder, err = lockHolder(f)
		f.Close() // this process holds no lock on the file, so the close releases nothing
		if err != nil {
			err = fmt.Errorf("%s: %v", lock, err)
		}
	} else if !errors.Is(oerr, fs.ErrNotExist) {
		err = oerr
	}
	if b, err := os.ReadFile(filepath.Join(state, "restart.json")); err == nil {
		var s restartStatus
		if json.Unmarshal(b, &s) == nil && s.Version == 1 {
			st = &s
		}
	}
	trusted = holder > 0 && st != nil && st.PID == holder && st.Start != "" && startTime(holder) == st.Start
	return holder, st, trusted, err
}

// rexCmd runs rex until the deadline.
func (v *ctlView) rexCmd(deadline time.Time, args ...string) ([]byte, error) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return v.rexCmdCtx(ctx, args...)
}

// rexCmdCtx runs rex until ctx ends.
func (v *ctlView) rexCmdCtx(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, v.rex, append([]string{"-S", v.cfg.Server, "--autostart=false", "--timeout", "3s"}, args...)...)
	cmd.WaitDelay = 500 * time.Millisecond // a killed rex whose child keeps the pipes open
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%v: %s", err, strings.Join(strings.Fields(errb.String()), " "))
	}
	return out.Bytes(), nil
}

// perform asks the hub of this config to perform action, through the hub's own Rex client: one
// client.perform, never retried or sent again. Its answer keeps the hub's error code (a
// *rex.RexError), which the rex CLI does not print. The dial, the hello and the call end at
// deadline, and the connection closes at once (CloseNow), so nothing outlives the deadline.
// timeout_ms is the time left, at most Rex's 30 s.
func (v *ctlView) perform(deadline time.Time, action string, args map[string]any) (json.RawMessage, error) {
	socket, err := rex.SocketPath(v.cfg.Server)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	r, err := rex.Dial(ctx, socket)
	if err != nil {
		return nil, err
	}
	defer r.CloseNow()
	ms := min(max(time.Until(deadline).Milliseconds(), 1), 30000)
	var res struct {
		Result json.RawMessage `json:"result"`
	}
	if args == nil {
		args = map[string]any{}
	}
	err = r.Call(ctx, "client.perform", map[string]any{"client_id": v.own.ID, "action": action, "args": args, "timeout_ms": ms}, &res)
	return res.Result, err
}

// performOutcome classifies the failure of one perform. definite: the hub answered with a code in
// refusals, so the request did not apply. Every other failure is unknown, and the request can still
// be delivered and apply (rex-protocol.md: a timeout does not cancel the action): a dial error, the
// local deadline, a lost connection (the client's own "disconnected"), a Rex code such as
// client_unavailable, and any code not in refusals. code is the answer's code, or "".
func performOutcome(err error, refusals []string) (code string, definite bool) {
	var re *rex.RexError
	if !errors.As(err, &re) {
		return "", false
	}
	return re.Code, slices.Contains(refusals, re.Code)
}

// hubIDs lists the client IDs of the connected lazyrex-hub clients.
func (v *ctlView) hubIDs(deadline time.Time) ([]string, error) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return v.hubIDsCtx(ctx)
}

func (v *ctlView) hubIDsCtx(ctx context.Context) ([]string, error) {
	out, err := v.rexCmdCtx(ctx, "client", "ls", "--json")
	if err != nil {
		return nil, err
	}
	var listing struct {
		Clients []struct {
			ClientID string `json:"client_id"`
			Info     struct {
				Label string `json:"label"`
			} `json:"info"`
			ConnectionState string `json:"connection_state"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		return nil, fmt.Errorf("rex client ls: %v", err)
	}
	var ids []string
	for _, c := range listing.Clients {
		if c.Info.Label == "lazyrex-hub" && c.ConnectionState == "connected" {
			ids = append(ids, c.ClientID)
		}
	}
	return ids, nil
}

// hubs lists the connected lazyrex-hub clients and asks each for its state (hub.info). A hub whose
// hub.info fails or does not answer in 5 s is listed with an empty state.
func (v *ctlView) hubs() ([]hubClient, error) {
	ids, err := v.hubIDs(time.Now().Add(5 * time.Second))
	if err != nil {
		return nil, err
	}
	var hubs []hubClient
	for _, id := range ids {
		h := hubClient{ID: id}
		if out, err := v.rexCmd(time.Now().Add(5*time.Second), "-C", id, "do", "hub.info"); err == nil {
			var info struct {
				State  string `json:"state"`
				PID    int    `json:"pid"`
				Exec   int    `json:"exec"`
				Commit string `json:"commit"`
			}
			if json.Unmarshal(out, &info) == nil {
				h.State, h.PID, h.Exec, h.Commit = info.State, info.PID, info.Exec, info.Commit
			}
		}
		hubs = append(hubs, h)
	}
	return hubs, nil
}

func (v *ctlView) wrapperText() string {
	s := v.status
	switch s.Phase {
	case "starting":
		return fmt.Sprintf("starting: the restart wrapper (pid %d) starts the hub (attempt %d)", s.PID, s.Attempt)
	case "backoff":
		wait := "soon"
		if t, err := time.Parse(time.RFC3339, s.NextStart); err == nil {
			wait = "in " + max(time.Until(t), 0).Round(time.Second).String()
		}
		return fmt.Sprintf("backoff: the restart wrapper (pid %d) restarts the hub %s (attempt %d; last exit %s)", s.PID, wait, s.Attempt, s.LastExit)
	case "exec":
		return fmt.Sprintf("relaunching: the restart wrapper (pid %d) runs its executable again (exec %d)", s.PID, s.ExecTarget)
	default: // running, or a stale phase: the wrapper holds the lock, and no hub of this state is connected
		return fmt.Sprintf("starting: the restart wrapper (pid %d) reports %s, but no hub of this state is seen", s.PID, s.Phase)
	}
}

func (v *ctlView) othersText() string {
	var parts []string
	for _, h := range v.others {
		if h.State == "" {
			parts = append(parts, fmt.Sprintf("another hub runs on this Rex server (client %s, state unknown: hub.info failed)", h.ID))
		} else {
			parts = append(parts, fmt.Sprintf("another hub runs on this Rex server (state %s); not this config's hub", h.State))
		}
	}
	return strings.Join(parts, "\n")
}

// statusText gives the exit status and the text. The first word is the phase.
func (v *ctlView) statusText() (int, string) {
	lines := []string{}
	add := func(s string) {
		if s != "" {
			lines = append(lines, s)
		}
	}
	unasked := false // a connected hub did not answer hub.info: it can be the hub of this state
	for _, h := range v.others {
		unasked = unasked || h.State == ""
	}
	code := ctlNotRunning
	switch {
	case v.localErr != nil:
		code = ctlUnknown
		add(fmt.Sprintf("unknown: %v", v.localErr))
	case v.ownTwice:
		code = ctlUnknown
		add(fmt.Sprintf("unknown: more than one connected hub reports the state %s", v.state))
	case v.own != nil:
		code = ctlRunning
		s := fmt.Sprintf("running: hub pid %d (client %s), state %s", v.own.PID, v.own.ID, v.state)
		if v.trusted {
			s += fmt.Sprintf("; restart wrapper pid %d", v.status.PID)
		}
		add(s)
		for _, l := range v.plugins {
			add(l)
		}
		for _, l := range v.agents {
			add(l)
		}
		for _, l := range v.remote {
			add(l)
		}
	case v.holder > 0 && !v.trusted:
		code = ctlUnknown
		add(fmt.Sprintf("unknown: a restart wrapper (pid %d) holds %s; its status file does not match", v.holder, v.state))
	case v.holder > 0:
		code = ctlWrapper
		add(v.wrapperText())
	// No wrapper holds the state. Only a complete listing of answered hubs shows that no hub runs.
	case v.rexErr != nil:
		code = ctlUnknown
		add("unknown: the Rex server cannot be asked, so a hub of this state cannot be ruled out")
	case unasked:
		code = ctlUnknown
		add("unknown: a connected hub did not answer hub.info; it can be the hub of this state")
	default:
		add("not running")
	}
	if code != ctlRunning && v.holder == 0 && v.status != nil && v.status.Phase == "gave_up" {
		add(fmt.Sprintf("the restart wrapper gave up at %s; last exit %s", v.status.Since, v.status.LastExit))
	}
	if v.rexErr != nil {
		add(fmt.Sprintf("the Rex server %s cannot be asked: %v", v.cfg.Server, v.rexErr))
	}
	add(v.othersText())
	return code, strings.Join(lines, "\n")
}

// stop stops the hub of this config with hub.admin.shutdown and waits until it is gone (and its
// wrapper, if one held the lock). It sends no signal, and it sends the request once: Rex can still
// deliver a request whose call failed, so only the hub's refusal ends stop early (performOutcome).
// Every other outcome waits for the hub to go, until the one deadline.
func (v *ctlView) stop(stdout io.Writer) int {
	code, text := v.statusText()
	if code != ctlRunning {
		fmt.Fprintln(stdout, text)
		switch code {
		case ctlNotRunning:
			return 0
		case ctlWrapper:
			fmt.Fprintln(stdout, "stop it in its pane: Ctrl-C, or close that pane")
		}
		return code
	}
	seen := stopSeen{listed: true, listedAt: time.Now()} // statusText saw the hub, before the send
	deadline := time.Now().Add(ctlStopWait)              // one bound for all of stop
	_, sendErr := v.perform(deadline, "hub.admin.shutdown", nil)
	if _, refused := performOutcome(sendErr, []string{"refused"}); refused {
		fmt.Fprintf(stdout, "hub.admin.shutdown: %v\n", sendErr)
		return 1
	}
	wrapped := v.holder > 0 // a holder whose status file does not match still owns the state
	for ; ; time.Sleep(200 * time.Millisecond) {
		// A look started this close to the deadline is cut by it, and its error says nothing about
		// the hub: report from the last look instead.
		if left := time.Until(deadline); left < ctlLookMin {
			time.Sleep(left)
			v.stopTimedOut(stdout, seen, sendErr)
			return 1
		}
		ids, err := v.hubIDs(deadline)
		seen.lastOK, seen.seenErr = err == nil, err
		if err == nil {
			seen.listed = slices.Contains(ids, v.own.ID)
			seen.lockHeld, seen.lockErr = false, nil
			if seen.listed {
				seen.listedAt = time.Now()
			} else if wrapped {
				holder, _, _, err := readRestart(v.state)
				seen.lockHeld, seen.lockErr = holder > 0, err
			}
			if !seen.listed && !seen.lockHeld && seen.lockErr == nil {
				fmt.Fprintln(stdout, "stopped")
				return 0
			}
		}
		if time.Now().After(deadline) {
			v.stopTimedOut(stdout, seen, sendErr)
			return 1
		}
	}
}

// ctlLookMin is the least time left before the deadline in which stop starts a client listing.
const ctlLookMin = 100 * time.Millisecond

// stopSeen is what stop last saw. The last successful listing sets listed (the hub client was in
// it, at listedAt) and, when the client was gone, lockHeld or lockErr (restart.lock could not be
// read). lastOK: the last look after the shutdown send was that successful listing; seenErr: the
// last look failed.
type stopSeen struct {
	listed, lastOK, lockHeld bool
	listedAt                 time.Time
	lockErr, seenErr         error
}

// stopTimedOut is what stop says at its deadline.
func (v *ctlView) stopTimedOut(stdout io.Writer, seen stopSeen, sendErr error) {
	fmt.Fprintf(stdout, "the hub did not stop within %g s; the shutdown request may still apply: run lazyrex-hub status\n", ctlStopWait.Seconds())
	switch {
	case seen.listed && seen.lastOK:
		fmt.Fprintln(stdout, "the hub client is still connected")
	case seen.listed:
		fmt.Fprintf(stdout, "the last successful check (%s ago) still listed the hub\n", time.Since(seen.listedAt).Round(100*time.Millisecond))
	case seen.lockHeld:
		fmt.Fprintf(stdout, "the restart wrapper still holds %s\n", v.state)
	case seen.lockErr != nil:
		fmt.Fprintf(stdout, "the hub client is gone, but its restart lock cannot be read: %v\n", seen.lockErr)
	}
	if seen.listed && sendErr != nil {
		fmt.Fprintf(stdout, "hub.admin.shutdown: %v\n", sendErr)
	}
	if seen.seenErr != nil {
		fmt.Fprintf(stdout, "the last failed look: %v\n", seen.seenErr)
	}
}

// restart relaunches the hub of this config inside its restart wrapper (docs/hub.md, "Restart").
// The checks before the send only read: a hub of this state, its wrapper (trusted, phase running)
// and exec n. Then it sends hub.admin.relaunch once, and never again: Rex can still deliver a
// request whose call failed. Only the hub's refusal ends restart early. Every other outcome is
// watched with fresh looks until the one deadline, and only a look decides:
//   - exit 0: the same wrapper process (pid and start time) is ready with a new hub at exec n+1;
//   - exit 1: the same wrapper is ready with a new hub at exec n, after a failed exec to n+1 (the
//     old version runs again);
//   - exit 4 at the deadline: not confirmed. A hub that cannot be seen never counts as restarted.
func (v *ctlView) restart(stdout io.Writer, n int, look func() *ctlView) int {
	code, text := v.statusText()
	switch {
	case code != ctlRunning:
		fmt.Fprintf(stdout, "%s\nrestart: no hub of this config runs; nothing was sent\n", text)
		return 1
	case !v.trusted || v.status.Phase != "running":
		fmt.Fprintf(stdout, "restart: the hub (pid %d) runs without a restart wrapper; stop it and start it again by hand; nothing was sent\n", v.own.PID)
		return 1
	case v.own.Exec != n:
		fmt.Fprintf(stdout, "restart: the hub (pid %d) runs exec %d, not %d: it was restarted already, or -expect-exec is wrong; nothing was sent\n", v.own.PID, v.own.Exec, n)
		return 1
	}
	old, wrapper := *v.own, *v.status
	deadline := time.Now().Add(ctlRestartWait) // one bound for all of restart
	_, sendErr := v.perform(deadline, "hub.admin.relaunch", map[string]any{"expect_exec": n})
	if _, refused := performOutcome(sendErr, []string{"refused", "invalid_input"}); refused {
		fmt.Fprintf(stdout, "hub.admin.relaunch: %v\n", sendErr)
		return 1
	}
	if sendErr != nil {
		fmt.Fprintf(stdout, "hub.admin.relaunch: %v; the request may still apply: watching the hub\n", sendErr)
	} else {
		fmt.Fprintf(stdout, "relaunching: hub pid %d, exec %d; restart wrapper pid %d\n", old.PID, n, wrapper.PID)
	}
	last := ""
	for ; time.Until(deadline) >= ctlLookMin; time.Sleep(200 * time.Millisecond) {
		seen := look()
		if code, text, done := restartOutcome(seen, old, wrapper, n); done {
			fmt.Fprintln(stdout, text)
			return code
		}
		_, last = seen.statusText()
	}
	fmt.Fprintf(stdout, "the restart is not confirmed within %g s: run lazyrex-hub status\n", ctlRestartWait.Seconds())
	if last != "" {
		fmt.Fprintf(stdout, "the last look: %s\n", strings.ReplaceAll(last, "\n", "; "))
	}
	return ctlUnknown
}

// restartOutcome reads one look after a relaunch of the hub old at exec n under the wrapper whose
// status was wrapper. It decides only when that wrapper process (same pid and start time) is ready
// with a new hub process of this state.
func restartOutcome(v *ctlView, old hubClient, wrapper restartStatus, n int) (code int, text string, done bool) {
	if v.localErr != nil || v.ownTwice || v.own == nil || !v.trusted {
		return 0, "", false
	}
	h, s := v.own, v.status
	if s.PID != wrapper.PID || s.Start != wrapper.Start || s.Phase != "running" || h.PID == old.PID || s.LastExec == nil || s.LastExec.Target != n+1 {
		return 0, "", false
	}
	switch {
	case s.LastExec.Outcome == "ok" && s.Exec == n+1 && h.Exec == n+1:
		return 0, fmt.Sprintf("restarted: hub pid %d (client %s), exec %d, commit %s; restart wrapper pid %d", h.PID, h.ID, h.Exec, h.Commit, s.PID), true
	case s.LastExec.Outcome == "failed" && s.Exec == n && h.Exec == n:
		return 1, fmt.Sprintf("the exec of the new version failed: %s\nthe old version runs again: hub pid %d (client %s), exec %d, commit %s; restart wrapper pid %d", s.LastExec.Error, h.PID, h.ID, h.Exec, h.Commit, s.PID), true
	}
	return 0, "", false
}

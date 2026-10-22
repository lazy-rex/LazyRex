package main

// lazyrex-hub stop|status -config <cfg> [-real-server] (docs/hub.md, "Stop and status"). Both
// read the config, then look at two things:
//   - the hub of this config: a connected Rex client labeled lazyrex-hub whose hub.info state is
//     the config's canonical state;
//   - the restart wrapper of that state: the holder of the fcntl lock on <state>/restart.lock,
//     and <state>/restart.json, trusted only when its pid is the holder and that pid has the
//     start time in the file.
// stop never sends a signal: a running hub gets hub.admin.shutdown (the admin guard applies), and
// a wrapper without a hub (starting, backoff) is stopped in its own pane.

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
	"regexp"
	"slices"
	"strings"
	"time"
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
	ID    string
	State string
	PID   int
}

func ctlMain(cmd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "hub configuration JSON (required)")
	realServer := fs.Bool("real-server", false, "the config names the user's real Rex server")
	if err := fs.Parse(args); err != nil || *cfgPath == "" || fs.NArg() > 0 {
		fmt.Fprintf(stderr, "usage: lazyrex-hub %s -config <cfg> [-real-server]\n", cmd)
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
		return out.Bytes(), &cliError{err: err, stderr: errb.String()}
	}
	return out.Bytes(), nil
}

// cliError is a failed rex command: its error and the stderr it printed.
type cliError struct {
	err    error
	stderr string
}

func (e *cliError) Error() string {
	return fmt.Sprintf("%v: %s", e.err, strings.Join(strings.Fields(e.stderr), " "))
}

func (e *cliError) Unwrap() error { return e.err }

// cliErrorLine is the last stderr line of a rex call that the target answered with an error
// (Rex 0.0.1792158932, observed): Error: stream response "<id>" failed: <message> (status=<n> code=<code>).
var cliErrorLine = regexp.MustCompile(`^Error: stream response "[^"]*" failed: (.*) \(status=(\d+) code=([a-z0-9_]+)\)$`)

// cliOutcome classifies the failure of one rex call. definite: the target answered with a code in
// refusals, so the request did not apply. Every other failure is unknown: a transport code, a code
// not in refusals, output that is not the CLI error line, a killed rex. The request can then still
// be delivered and apply (rex-protocol.md: a timeout does not cancel the action). code is the
// answer's code, or "".
func cliOutcome(err error, refusals []string) (code string, definite bool) {
	var ce *cliError
	if err == nil || !errors.As(err, &ce) {
		return "", false
	}
	last := ""
	for _, l := range strings.Split(ce.stderr, "\n") {
		if strings.TrimSpace(l) != "" {
			last = strings.TrimSpace(l)
		}
	}
	m := cliErrorLine.FindStringSubmatch(last)
	if m == nil {
		return "", false
	}
	return m[3], slices.Contains(refusals, m[3])
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
				State string `json:"state"`
				PID   int    `json:"pid"`
			}
			if json.Unmarshal(out, &info) == nil {
				h.State, h.PID = info.State, info.PID
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
// deliver a request whose call failed, so only the hub's refusal ends stop early (cliOutcome).
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
	deadline := time.Now().Add(ctlStopWait) // one bound for all of stop
	out, sendErr := v.rexCmd(deadline, "-C", v.own.ID, "do", "hub.admin.shutdown")
	if _, refused := cliOutcome(sendErr, []string{"refused"}); refused {
		fmt.Fprintf(stdout, "hub.admin.shutdown: %v %s\n", sendErr, strings.TrimSpace(string(out)))
		return 1
	}
	wrapped := v.holder > 0 // a holder whose status file does not match still owns the state
	// The last successful listing decides what stop says at the deadline: listed (the hub client
	// was in it, at listedAt) or gone; lockHeld: gone, but the restart lock holder stays.
	listed, listedAt, lockHeld := true, time.Now(), false // statusText saw the hub just now
	// seenErr is the last look, when it failed.
	var seenErr error
	for ; ; time.Sleep(200 * time.Millisecond) {
		// A look started this close to the deadline is cut by it, and its error says nothing about
		// the hub: report from the last look instead.
		if left := time.Until(deadline); left < ctlLookMin {
			time.Sleep(left)
			v.stopTimedOut(stdout, listed, seenErr == nil, lockHeld, listedAt, sendErr, out, seenErr)
			return 1
		}
		ids, err := v.hubIDs(deadline)
		seenErr = err
		if err == nil {
			listed = slices.Contains(ids, v.own.ID)
			lockHeld = false
			if listed {
				listedAt = time.Now()
			} else if wrapped {
				if holder, _, _, err := readRestart(v.state); holder > 0 || err != nil {
					lockHeld = true
					seenErr = err
				}
			}
			if !listed && !lockHeld {
				fmt.Fprintln(stdout, "stopped")
				return 0
			}
		}
		if time.Now().After(deadline) {
			v.stopTimedOut(stdout, listed, err == nil, lockHeld, listedAt, sendErr, out, seenErr)
			return 1
		}
	}
}

// ctlLookMin is the least time left before the deadline in which stop starts a client listing.
const ctlLookMin = 100 * time.Millisecond

// stopTimedOut is what stop says at its deadline, from the last successful listing: listed (at
// listedAt), and lastOK when that listing was also the last look.
func (v *ctlView) stopTimedOut(stdout io.Writer, listed, lastOK, lockHeld bool, listedAt time.Time, sendErr error, out []byte, seenErr error) {
	fmt.Fprintf(stdout, "the hub did not stop within %g s; the shutdown request may still apply: run lazyrex-hub status\n", ctlStopWait.Seconds())
	switch {
	case listed && lastOK:
		fmt.Fprintln(stdout, "the hub client is still connected")
	case listed:
		fmt.Fprintf(stdout, "the last successful check (%s ago) still listed the hub\n", time.Since(listedAt).Round(100*time.Millisecond))
	case lockHeld:
		fmt.Fprintf(stdout, "the restart wrapper still holds %s\n", v.state)
	}
	if listed && sendErr != nil {
		fmt.Fprintf(stdout, "hub.admin.shutdown: %v %s\n", sendErr, strings.TrimSpace(string(out)))
	}
	if seenErr != nil {
		fmt.Fprintf(stdout, "the last failed look: %v\n", seenErr)
	}
}

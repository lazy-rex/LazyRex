package main

// The fetching phase of add and update, and the git runner of the plugin manager (step 4 C2: the
// integrated runner revision, approved by the hub lead and codex-27). Every git command of a
// manager phase runs through the fetch child (fetchchild.go), so no git process outlives its
// phase, also after a hub crash.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// opFetchLimit bounds the fetching phase: every git command of it (step 4 plan, item 3b).
const opFetchLimit = 120 * time.Second

// fetchLimit is opFetchLimit; a test shortens it.
var fetchLimit = opFetchLimit

// Test hooks, nil in production: fetchChildRecord gets the watcher and the gate of each fetch
// child of the manager, and fetchChildStderr wraps the stderr capture of a git command.
var (
	fetchChildRecord func(watcher, gate int)
	fetchChildStderr func(args []string, w io.Writer) io.Writer
)

// errFetchStderrOpen is a fetch whose standard error copy did not end within the bound.
var errFetchStderrOpen = errors.New("the git error output did not end")

// fetchStopBound is the bound of a cancelled fetch: fetchGrace, then the hard stop (5 s). Every
// wait of fetchChildGit after a cancel ends at it.
func fetchStopBound() time.Duration { return fetchGrace + time.Second }

// outputGate guards the stream callback of one fetch child: it runs at most once, and a runner
// that has returned never starts it. done closes when the callback returns; err is read only
// after done.
type outputGate struct {
	mu        sync.Mutex
	started   bool
	abandoned bool
	read      func(io.Reader) error
	done      chan struct{}
	err       error
}

func (g *outputGate) run(r io.Reader) error {
	g.mu.Lock()
	if g.abandoned {
		g.mu.Unlock()
		return errFetchOutputOpen
	}
	g.started = true
	g.mu.Unlock()
	g.err = g.read(r)
	if g.err == nil {
		io.Copy(io.Discard, r) // the rest of the stream (a tar ends with padding), as execGit drains it
	}
	close(g.done)
	return g.err
}

// end reports whether the callback still runs after wait, and the callback's error when it
// returned. A callback that has not started never starts.
func (g *outputGate) end(wait time.Duration) (running bool, err error) {
	g.mu.Lock()
	if !g.started {
		g.abandoned = true
		g.mu.Unlock()
		return false, nil
	}
	g.mu.Unlock()
	select {
	case <-g.done:
		return false, g.err
	default:
	}
	if wait > 0 {
		select {
		case <-g.done:
			return false, g.err
		case <-time.After(wait):
		}
	}
	return true, nil
}

// fetchChildGit is the git runner of the manager's phases: each command runs in a fetch child
// with GIT_TERMINAL_PROMPT=0 and a 4 KiB stderr capture. It returns only after the stream callback
// has returned, or with *outputRunningError when the callback still runs at the end of the stop
// bound (measured from the cancel). The error order: the callback still runs, the callback's own
// error, the context, the stderr copy did not end, a fetch child error, then git's standard
// error, else the exit.
func (h *Hub) fetchChildGit(opID, phase string) gitRunner {
	return func(ctx context.Context, dir string, out func(io.Reader) error, args ...string) ([]byte, error) {
		var stdout bytes.Buffer
		read := out
		if read == nil {
			read = func(r io.Reader) error { _, err := io.Copy(&stdout, r); return err }
		}
		var cancelled atomic.Int64 // the time of the cancel (UnixNano); 0: no cancel
		unwatch := context.AfterFunc(ctx, func() { cancelled.Store(time.Now().UnixNano()) })
		defer unwatch()
		g := &outputGate{read: read, done: make(chan struct{})}
		stderr := &limitedBuffer{max: gitErrLimit}
		var sink io.Writer = stderr
		if fetchChildStderr != nil {
			sink = fetchChildStderr(args, stderr)
		}
		res, err := runFetchChild(ctx, fetchCmd{Argv: append([]string{"git"}, args...), Dir: dir, Env: []string{"GIT_TERMINAL_PROMPT=0"}, Stderr: sink, Output: g.run},
			func(_ context.Context, watcher, gate int) error {
				if fetchChildRecord != nil {
					fetchChildRecord(watcher, gate)
				}
				h.ev.emit("fetch_started", map[string]any{"op_id": opID, "phase": phase})
				return nil
			})
		stopped := time.Now()
		if at := cancelled.Load(); at != 0 {
			stopped = time.Unix(0, at)
		}
		running, xerr := g.end(time.Until(stopped.Add(fetchStopBound())))
		switch {
		case running:
			return nil, &outputRunningError{}
		case xerr != nil:
			return nil, xerr
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case res.StderrOpen:
			return nil, errFetchStderrOpen
		case err != nil:
			return nil, err
		case !res.OK():
			if s := strings.TrimSpace(stderr.String()); s != "" {
				return nil, errors.New(s)
			}
			switch {
			case res.Stopped:
				return nil, errors.New("the fetch was stopped")
			case res.OutputErr != nil:
				return nil, res.OutputErr
			case res.GitStatus >= 0:
				return nil, fmt.Errorf("exit status %d", res.GitStatus) // the text of exec.ExitError, as execGit gives it
			}
			return nil, fmt.Errorf("the fetch watcher ended with %v", res.WatcherStatus)
		}
		if out != nil {
			return nil, nil
		}
		return stdout.Bytes(), nil
	}
}

// opCmd are the command options of an op's phase: the manager's runner, unless a test set one.
func (h *Hub) opCmd(op *pluginOp, phase string) pluginCmdOpts {
	o := h.pops.cmd
	if o.git == nil {
		o.git = h.fetchChildGit(op.ID, phase)
	}
	return o
}

// opFetch is the fetching phase: it fetches the stored target into the plugin cache and stores
// the pin (the lock entry). A stored pin whose directory still verifies is not fetched again. A
// hub stop stores nothing and keeps the phase, so the replay fetches again into a new directory.
// A failed fetch changed neither the lock nor the managed file, so the op ends failed.
func (h *Hub) opFetch(ctx context.Context, op *pluginOp) error {
	var t opResolved
	if err := json.Unmarshal([]byte(op.Target), &t); err != nil {
		return h.setOpPhase(ctx, op, "failed", "", "fetch: the stored target does not read: "+err.Error())
	}
	e := &configEntry{Source: t.Source}
	if err := e.checkSource(""); err != nil {
		return h.setOpPhase(ctx, op, "failed", "", "fetch: "+err.Error())
	}
	if op.Pin != "" {
		var le lockEntry
		if json.Unmarshal([]byte(op.Pin), &le) == nil {
			if _, _, err := lockedDir(h.cfg.path, h.cfg.State, &pluginLock{Plugins: map[string]*lockEntry{t.Source: &le}}, e); err == nil {
				return h.setOpPhase(ctx, op, op.nextPhase("fetching"), fmt.Sprintf("fetched %s %s", le.Name, le.Commit), "")
			}
		}
	}
	tmpRoot := filepath.Join(h.cfg.State, "plugins", ".tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return err
	}
	fctx, cancel := context.WithTimeout(ctx, fetchLimit)
	cfg := &pluginCmdConfig{path: h.cfg.path, state: h.cfg.State}
	le, err := fetchAt(fctx, cfg, h.opCmd(op, "fetching"), tmpRoot, e.githubRef(), t.FetchRef, t.Commit, t.RefKind, t.Tag, t.RefKind == "commit")
	timedOut := errors.Is(fctx.Err(), context.DeadlineExceeded)
	cancel()
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil && timedOut:
		return h.setOpPhase(ctx, op, "failed", "", fmt.Sprintf("fetch %s: not done within %v; the lock and the managed file are unchanged", t.Source, fetchLimit))
	case err != nil:
		return h.setOpPhase(ctx, op, "failed", "", fmt.Sprintf("fetch %s: %v; the lock and the managed file are unchanged", t.Source, err))
	}
	if h.opFault("plugin-op-after-fetch") {
		return errOpCrash
	}
	b, _ := json.Marshal(le)
	if _, err := h.db.ExecContext(ctx, `UPDATE plugin_ops SET pin = ?, updated = ? WHERE op_id = ?`, string(b), time.Now().UnixMilli(), op.ID); err != nil {
		return err
	}
	op.Pin = string(b)
	return h.setOpPhase(ctx, op, op.nextPhase("fetching"), fmt.Sprintf("fetched %s %s", le.Name, le.Commit), "")
}

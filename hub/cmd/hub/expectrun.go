package main

// expect_run (docs/plugin-sources.md, section 14): hub.admin.replace and hub.admin.start can name
// the exact run that the caller expects to start: the source string (owner, repo, subdir and ref
// for a GitHub source), and for a GitHub source the commit and the digest of its cache entry. The
// check runs inside admission, on the candidate that the spawn uses, after the resolution and
// before any retirement or spawn. A different candidate is refused with code candidate_changed,
// and nothing changes. A caller that writes the lock (plugin use, the plugin manager) so cannot
// start a candidate that another writer put in the lock after its write.

import (
	"context"
	"fmt"
	"strings"
)

type expectRun struct {
	Source, Commit, Digest string
}

func (x *expectRun) String() string {
	if x.Commit == "" {
		return x.Source
	}
	return fmt.Sprintf("%s at %s (%s)", x.Source, x.Commit, x.Digest)
}

// runText names a run in the expect_run terms.
func runText(r *pluginRun) string {
	return (&expectRun{r.Source, r.Commit, r.Digest}).String()
}

// matches reports whether the run r is the expected run.
func (x *expectRun) matches(r *pluginRun) bool {
	return r != nil && r.Source == x.Source && r.Commit == x.Commit && r.Digest == x.Digest
}

// parseExpectRun reads the optional "expect_run" field: nil when it is absent. A GitHub source
// needs its commit (40 lowercase hex) and digest (sha256:<64 hex>); a local source has neither.
func parseExpectRun(args map[string]any) (*expectRun, error) {
	v, ok := args["expect_run"]
	if !ok {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, bad("invalid: expect_run must be an object")
	}
	for k, f := range m {
		if k != "source" && k != "commit" && k != "digest" {
			return nil, bad("invalid: expect_run: unknown field %s", k)
		}
		if _, ok := f.(string); !ok {
			return nil, bad("invalid: expect_run.%s must be a string", k)
		}
	}
	x := &expectRun{}
	x.Source, _ = m["source"].(string)
	x.Commit, _ = m["commit"].(string)
	x.Digest, _ = m["digest"].(string)
	if x.Source == "" {
		return nil, bad("invalid: expect_run needs a source")
	}
	if strings.HasPrefix(x.Source, "github:") {
		if !commitID.MatchString(x.Commit) || !lockDigest.MatchString(x.Digest) {
			return nil, bad("invalid: expect_run of a GitHub source needs commit (40 lowercase hex) and digest (sha256:<64 hex>)")
		}
	} else if x.Commit != "" || x.Digest != "" {
		return nil, bad("invalid: expect_run of a local source has no commit and no digest")
	}
	return x, nil
}

// candidateChanged is the refusal of a candidate that is not the expected run.
func candidateChanged(name string, cand *pluginRun, x *expectRun, unchanged string) error {
	return refuse("candidate_changed", "plugin %s: the candidate is %s, not %s; %s", name, runText(cand), x, unchanged)
}

// startExpected is the admission of a start with an expected run x (hub.admin.start expect_run),
// after the resolution of the start path (checkStart): the candidate cand is the one that it
// spawns. It admits once, in the order of a replace, and it never waits:
//  1. cand must be x, else candidate_changed.
//  2. In one h.mu section: the entry must still be the resolved one, else refused; each dependency
//     must be registered, else dependency_not_ready; then the record becomes desired running, and
//     the save must succeed. A refusal or a failed save leaves the desired state as it was, and
//     no waiting state.
//  3. A retained live instance whose run is x is kept. Else the recorded instance is retired.
//  4. Spawn from cand. A dependency that stops after step 2 does not make this start wait (as in a
//     replace): no later wake starts another candidate.
//
// The caller holds pl.lifecycle and the start lease.
func (h *Hub) startExpected(ctx context.Context, pl *Plugin, keepLive bool, x *expectRun, e *configEntry, cand *pluginRun, lease *startLease) error {
	if !x.matches(cand) {
		return candidateChanged(pl.Name, cand, x, "nothing was retired or spawned")
	}
	h.mu.Lock()
	if pl.entry != e {
		// The entry was removed after the resolution. Nothing writes the kept record here, as in
		// startOrWait. Today a removal is a hub.plugins.reload request, and the hub serves one
		// request at a time, so this guards a later concurrent writer.
		h.mu.Unlock()
		return refuse("refused", "plugin %s: its entry was removed during the start; nothing was retired or spawned", pl.Name)
	}
	var missing string
	for _, d := range cand.Depends {
		if !h.registeredLocked(d) {
			missing = d
			break
		}
	}
	if missing != "" {
		h.mu.Unlock()
		return refuse("dependency_not_ready", "plugin %s: dependency %s is not registered; nothing was retired or spawned", pl.Name, missing)
	}
	prev := pl.Desired
	pl.Desired = "running"
	if err := h.savePlugin(ctx, pl); err != nil {
		pl.Desired = prev
		h.mu.Unlock()
		return fmt.Errorf("plugin %s: the desired state was not saved: %w; nothing was retired or spawned", pl.Name, err)
	}
	pl.cancelRespawnLocked() // the start passed its refusals: it owns the plugin now (respawn.go)
	keep := keepLive && pl.ClientID != "" && alive(pl.ChildPID, pl.ChildStart) && x.matches(&pl.run)
	pl.state, pl.refusal, pl.waiting = "", "", nil // admitted: not refused, not waiting
	h.mu.Unlock()
	if keep {
		h.kickWaiting()
		go h.changed("plugins")
		return nil
	}
	if pl.BlockID != "" {
		ok, err := h.retire(ctx, pl)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("plugin %s: the recorded instance is not retired (see the plugin_conflict or plugin_stopped event)", pl.Name)
		}
	}
	return h.spawn(ctx, pl, cand, lease)
}

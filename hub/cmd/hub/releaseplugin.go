package main

// The steps of a plugin release (docs/releasing.md; plan c4): one GitHub plugin source P moves from
// its lock pin FROM to a target commit TO, while the hub keeps running. The hub binary and its SDK
// directory do not change: freeze records them, and every later step checks that the running hub
// is still that hub before it mutates anything. The steps are freeze, precheck, backup, pull,
// replace, verify and rollback; the dev pane steps of a hub release (stop, checkout, start, resume)
// are not steps of a plugin release.

import (
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// relPluginHubFile is the hub that precheck of a plugin release saw: its pid, start time, client
// and hub_started sequence.
const relPluginHubFile = "plugin-hub.json"

// runtimeField reports whether the hub reads a manifest field at run time. A change of one of them
// between FROM and TO is a reason for the full rehearsal tier. "name" must not change, and "test"
// is not read at run time; any other field that changes is a reason too.
func runtimeField(k string) bool {
	switch k {
	case "main", "sdk", "events", "notice", "depends":
		return true
	}
	return false
}

// devHubPath is the hub binary that dev.sh runs: <state>/dev/hub.
func (r *releaser) devHubPath() string { return filepath.Join(r.cfg.state, "dev", "hub") }

// gitIn runs git in dir and returns its trimmed stdout.
func (r *releaser) gitIn(dir string, args ...string) (string, error) {
	argv := append([]string{"git", "-C", dir}, args...)
	o, e, err := r.runCmd(relCmd{Argv: argv})
	if err != nil {
		return "", cmdError(argv, e, err)
	}
	return strings.TrimSpace(string(o)), nil
}

// pluginSource finds the source of the spec's plugin: exactly one config or managed entry whose
// lock entry has that name. It must be a GitHub source.
func (r *releaser) pluginSource(l *pluginLock) (string, *lockEntry, error) {
	managed, err := parseManaged(r.cfg.managed)
	if err != nil {
		return "", nil, relFail("the managed file: %v", err)
	}
	var src string
	var found *lockEntry
	for _, e := range addManaged(r.cfg.config, managed) {
		le := l.Plugins[e.Source]
		if le == nil || le.Name != r.spec.Plugin {
			continue
		}
		if !e.github {
			return "", nil, relFail("%s is not a GitHub source", e.Source)
		}
		if found != nil {
			return "", nil, relFail("two entries name the plugin %s: %s and %s", r.spec.Plugin, src, e.Source)
		}
		src, found = e.Source, le
	}
	if found == nil {
		return "", nil, relFail("no config or managed entry with a lock entry names the plugin %s; the release tool releases only locked GitHub sources", r.spec.Plugin)
	}
	return src, found, nil
}

// reachable checks that TO is reachable from the ref of the source: a branch ref needs TO to be an
// ancestor of the remote branch head, and a tag ref needs TO to be the tag's commit. It fetches the
// ref (commits only) into <run>/reach.git.
func (r *releaser) reachable(le *lockEntry, to string) error {
	dir := filepath.Join(r.run, "reach.git")
	if _, err := r.gitIn(r.run, "init", "-q", "--bare", dir); err != nil {
		return err
	}
	url := "https://github.com/" + le.Owner + "/" + le.Repo + ".git"
	switch le.RefKind {
	case "tag":
		ref := "refs/tags/" + le.Ref
		if _, err := r.gitIn(dir, "fetch", "-q", "--filter=blob:none", url, "+"+ref+":"+ref); err != nil {
			return err
		}
		c, err := r.gitIn(dir, "rev-parse", ref+"^{commit}")
		if err != nil {
			return err
		}
		if c != to {
			return relFail("the tag %s of %s/%s is %s, not TO %s", le.Ref, le.Owner, le.Repo, c, to)
		}
	case "branch":
		ref := "refs/remotes/origin/" + le.Ref
		if _, err := r.gitIn(dir, "fetch", "-q", "--filter=blob:none", url, "+refs/heads/"+le.Ref+":"+ref); err != nil {
			return err
		}
		argv := []string{"git", "-C", dir, "merge-base", "--is-ancestor", to, ref}
		_, e, err := r.runCmd(relCmd{Argv: argv})
		switch {
		case err == nil:
		case exitCode(err) == 1 && len(strings.TrimSpace(string(e))) == 0:
			return relFail("TO %s is not on the branch %s of %s/%s", to, le.Ref, le.Owner, le.Repo)
		default:
			return relFail("TO %s is not reachable from the branch %s of %s/%s: %s", to, le.Ref, le.Owner, le.Repo, strings.TrimSpace(string(e)))
		}
	default:
		return relFail("the ref kind %q of %s/%s is not a branch or a tag", le.RefKind, le.Owner, le.Repo)
	}
	return nil
}

// syncTo syncs the commit-form source of TO in a scratch state under the run directory (as
// digestOf), and returns its digest and its manifest. Tests replace it (no network).
func (r *releaser) syncTo(source string) (string, []byte, error) {
	if r.syncManifest != nil {
		return r.syncManifest(source)
	}
	d, err := r.digestOf(source)
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Join(r.run, "digest")
	l, err := readLock(lockPath(filepath.Join(dir, "config.json"), ""))
	if err != nil {
		return "", nil, err
	}
	e := l.Plugins[source]
	if e == nil {
		return "", nil, relFail("the sync of %s has no lock entry", source)
	}
	m, err := readManifestBytes(filepath.Join(dir, "state", "plugins", e.Dir, e.Subdir))
	if err != nil {
		return "", nil, relFail("the manifest of %s: %v", source, err)
	}
	return d, m, nil
}

// canonJSON is a JSON value with sorted object keys, for a comparison.
func canonJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// manifestReasons compares the manifests of FROM and TO. A name change, or a manifest that cannot
// be parsed, is a refusal. It returns the sdk field of each side and the tier reasons.
func manifestReasons(from, to []byte) (fromSDK, toSDK string, reasons []string, err error) {
	var a, b map[string]json.RawMessage
	if err := json.Unmarshal(from, &a); err != nil {
		return "", "", nil, relFail("the manifest at FROM: %v", err)
	}
	if err := json.Unmarshal(to, &b); err != nil {
		return "", "", nil, relFail("the manifest at TO: %v", err)
	}
	str := func(m map[string]json.RawMessage, k string) string {
		var s string
		json.Unmarshal(m[k], &s)
		return s
	}
	if str(a, "name") != str(b, "name") {
		return "", "", nil, relFail("the plugin name changes from %q to %q", str(a, "name"), str(b, "name"))
	}
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var names []string
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if k == "name" || k == "test" {
			continue
		}
		_, inA := a[k]
		_, inB := b[k]
		if inA == inB && canonJSON(a[k]) == canonJSON(b[k]) {
			continue
		}
		if runtimeField(k) {
			reasons = append(reasons, fmt.Sprintf("the manifest field %s changes", k))
		} else {
			reasons = append(reasons, fmt.Sprintf("the manifest field %s changes (a field the release tool does not know)", k))
		}
	}
	return str(a, "sdk"), str(b, "sdk"), reasons, nil
}

// buildSettings returns the build record of a Go binary: its Go version, its main module, every
// dependency with its version and sum, and every build setting, one "key value" line each.
func (r *releaser) buildSettings(path string) ([]string, error) {
	if r.buildRecord != nil {
		return r.buildRecord(path)
	}
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := []string{"go " + bi.GoVersion, "path " + bi.Path, "mod " + bi.Main.Path + " " + bi.Main.Version}
	for _, d := range bi.Deps {
		out = append(out, fmt.Sprintf("dep %s %s %s", d.Path, d.Version, d.Sum))
	}
	for _, s := range bi.Settings {
		out = append(out, "build "+s.Key+"="+s.Value)
	}
	return out, nil
}

// argvOfPID is the argv of a process; tests replace it. A failed or empty read is an error: it
// does not prove that the process has no -sdk flag (review R15).
func (r *releaser) argvOfPID(pid int) ([]string, error) {
	read := r.argvOf
	if read == nil {
		read = procArgv
	}
	argv, err := read(pid)
	if err == nil && len(argv) == 0 {
		err = errors.New("the argv is empty")
	}
	return argv, err
}

// sdkArg is the value of the -sdk flag in a hub argv, "" without one.
func sdkArg(argv []string) string {
	for i, a := range argv {
		switch {
		case (a == "-sdk" || a == "--sdk") && i+1 < len(argv):
			return argv[i+1]
		case strings.HasPrefix(a, "-sdk="):
			return strings.TrimPrefix(a, "-sdk=")
		case strings.HasPrefix(a, "--sdk="):
			return strings.TrimPrefix(a, "--sdk=")
		}
	}
	return ""
}

// liveHub reads the running hub of a plugin release: the hub of the last hub_started event, which
// runs <state>/dev/hub, a clean build. It returns the hub, the binary's commit and sha256, and the
// hub's -sdk directory.
func (r *releaser) liveHub() (h *relHub, commit, sum, sdk string, err error) {
	if h, err = r.currentHub(); err != nil {
		return nil, "", "", "", err
	}
	dev := r.devHubPath()
	if exe := r.procExe(h.PID); exe != dev {
		return nil, "", "", "", relFail("the hub pid %d runs %q, not %s", h.PID, exe, dev)
	}
	read := r.buildInfo
	if read == nil {
		read = readBuildInfo
	}
	rev, modified, err := read(dev)
	if err != nil {
		return nil, "", "", "", relFail("the build information of %s: %v", dev, err)
	}
	if modified {
		return nil, "", "", "", relFail("%s is not a clean build (vcs.modified is true)", dev)
	}
	if sum, err = sha256File(dev); err != nil {
		return nil, "", "", "", relFail("the sha256 of %s: %v", dev, err)
	}
	argv, err := r.argvOfPID(h.PID)
	if err != nil {
		return nil, "", "", "", relFail("the argv of the hub pid %d: %v; its -sdk directory is unknown", h.PID, err)
	}
	return h, rev, sum, sdkArg(argv), nil
}

func relPluginFreeze(r *releaser) error {
	if r.branch != "" {
		return relFail("-branch is for a hub release; a plugin release uses its pinned commit")
	}
	l, err := readLock(r.cfg.lock)
	if err != nil {
		return err
	}
	src, le, err := r.pluginSource(l)
	if err != nil {
		return err
	}
	if le.Owner == "lazy-rex" && le.Repo == "LazyRex" && le.Subdir == "plugins/lazyrex" {
		return relFail("%s is the home plugin: a hub release moves it", src)
	}
	if le.RefKind == "commit" {
		return relFail("%s has a commit ref: the config holds the pin, and a pull cannot move it", src)
	}
	to := r.spec.To
	if to == le.Commit {
		return relFail("TO %s is FROM, the lock pin of %s: a release without a change is not supported", to, src)
	}
	c := &relCandidate{Kind: "plugin", Source: src, Name: le.Name, From: relPin{le.Commit, le.Digest}}
	if c.Peer, err = onePeer(r.cfg.lock); err != nil {
		return err
	}
	base, err := readPeers(r.cfg.lock)
	if err != nil {
		return err
	}
	if b := base.Peers[c.Peer][src]; b.Commit != le.Commit || b.Digest != le.Digest {
		return relFail("the base of %s is not its lock pin", src)
	}
	// The hub that will run TO: freeze reads the running hub and its binary.
	h, commit, sum, sdk, err := r.liveHub()
	if err != nil {
		return err
	}
	c.HubCommit, c.HubSHA256, c.SDKDir = commit, sum, sdk
	if c.HubBuild, err = r.buildSettings(r.devHubPath()); err != nil {
		return relFail("the build record of %s: %v", r.devHubPath(), err)
	}
	if sdk != "" {
		if c.SDKDigest, err = dirDigest(sdk); err != nil {
			return relFail("the SDK directory %s of the hub: %v", sdk, err)
		}
	}
	if err := r.reachable(le, to); err != nil {
		return err
	}
	cs := strings.SplitN(src, "@", 2)[0] + "@" + to
	d, toManifest, err := r.syncTo(cs)
	if err != nil {
		return err
	}
	c.To = relPin{to, d}
	fromManifest, err := readManifestBytes(filepath.Join(r.cfg.state, "plugins", le.Dir, le.Subdir))
	if err != nil {
		return relFail("the manifest of %s at FROM: %v", src, err)
	}
	if c.FromSDK, c.ToSDK, c.Reasons, err = manifestReasons(fromManifest, toManifest); err != nil {
		return err
	}
	if sdk == "" && (c.FromSDK != "" || c.ToSDK != "") {
		return relFail("%s uses the SDK, and the hub (pid %d) runs without -sdk", src, h.PID)
	}
	if c.Pins, err = lockPinLines(r.cfg.lock); err != nil {
		return err
	}
	if c.Files, err = r.fileHashes(); err != nil {
		return err
	}
	if c.LockPins, err = lockPinsHash(r.cfg.lock); err != nil {
		return err
	}
	fwd, back, err := pinDocs(c, le)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.run, relFwdDoc), fwd, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.run, relBackDoc), back, 0o600); err != nil {
		return err
	}
	if err := r.verifyLists(c, l); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(r.run, relCandidateFile), c); err != nil {
		return err
	}
	r.say("frozen: %s %s (%s -> %s), hub %s sha256 %s, sdk %q %s, tier reasons %q", c.Name, src, c.From.Commit, to, commit, sum, sdk, c.SDKDigest, c.Reasons)
	return nil
}

// checkPluginHub checks that the running hub is the frozen hub of a plugin release: it runs
// <state>/dev/hub with the frozen sha256, a clean build of the frozen commit, and its -sdk
// directory has the frozen digest. It runs before every mutation and in verify.
//   - mode "precheck" only checks; precheck saves the hub after all its checks pass.
//   - mode "forward" refuses a hub that started after precheck (another generation).
//   - mode "rollback" accepts another generation of the same binary: it reports it, journals it,
//     and decides from the real state.
func (r *releaser) checkPluginHub(c *relCandidate, mode string) (*relHub, error) {
	h, commit, sum, sdk, err := r.liveHub()
	if err != nil {
		return nil, err
	}
	if sum != c.HubSHA256 || commit != c.HubCommit {
		return nil, relFail("the hub runs %s (sha256 %s, commit %s), not the frozen hub (sha256 %s, commit %s); stop and report", r.devHubPath(), sum, commit, c.HubSHA256, c.HubCommit)
	}
	if sdk != c.SDKDir {
		return nil, relFail("the hub runs with -sdk %q, not the frozen %q; stop and report", sdk, c.SDKDir)
	}
	if sdk != "" {
		d, err := dirDigest(sdk)
		if err != nil {
			return nil, relFail("the SDK directory %s: %v", sdk, err)
		}
		if d != c.SDKDigest {
			return nil, relFail("the SDK directory %s has the digest %s, not the frozen %s; stop and report", sdk, d, c.SDKDigest)
		}
	}
	if mode == "precheck" {
		return h, nil
	}
	saved := r.readHub(relPluginHubFile)
	if saved == nil {
		return nil, relFail("no hub of precheck in %s (run precheck first)", relPluginHubFile)
	}
	if saved.PID == h.PID && saved.Start == h.Start && saved.Client == h.Client {
		return h, nil
	}
	if mode == "forward" {
		return nil, relFail("the hub restarted after precheck (pid %d -> %d, %s -> %s); run rollback", saved.PID, h.PID, saved.Client, h.Client)
	}
	r.say("the hub restarted after precheck (pid %d -> %d); the binary is the frozen one: rollback decides from the real state", saved.PID, h.PID)
	return h, r.appendJournal("hub-generation", "end", map[string]any{"precheck": saved, "now": h})
}

// rexFailure is the message and the code of a failed rex CLI call, from its stderr line
// "Error: ... failed: <message> (status=<n> code=<code>)" (observed on Rex 0.0.1792158932, lab).
var rexFailure = regexp.MustCompile(`failed: (.*) \(status=\d+ code=([a-z_]+)\)\s*$`)

// pluginClient is the registered instance of a plugin on the hub client: its hub.plugins entry and
// the client id of its plugin_registered event.
func (r *releaser) pluginClient(client, name string) (*relEntryState, string, error) {
	_, after, err := hubEvents(r.cfg.state)
	if err != nil {
		return nil, "", err
	}
	entries, _, err := r.hubPlugins(client)
	if err != nil {
		return nil, "", err
	}
	e := entryOf(entries, name)
	if e == nil {
		return nil, "", relFail("hub.plugins has no entry %s", name)
	}
	if !registeredNow(*e, after) {
		return nil, "", relFail("%s is not running and registered by its current instance (state %s, instance %q)", name, e.State, e.InstanceID)
	}
	var id string
	for _, ev := range after {
		if ev["event"] == "plugin_registered" && ev["plugin"] == name && ev["instance_id"] == e.InstanceID {
			id, _ = ev["client_id"].(string)
		}
	}
	if id == "" {
		return nil, "", relFail("no plugin_registered event names the client of %s instance %s", name, e.InstanceID)
	}
	return e, id, nil
}

// startEnded is the start barrier of a plugin (plan c4 v3 section 1, probe A1). The SDK registers
// the plugin, then runs its migrations and start functions in the same Rex handler, and a Rex
// client answers one request at a time. So view.seen from the release tool (not the hub) gets the
// SDK refusal "Only the hub calls view.seen" only after that handler has ended. Then the instance
// must still be the same, registered instance. Every other answer fails; a timeout is sent again,
// at most 4 times. A plugin without "sdk": "0" has no such contract: the start completion is not
// established, and the step says so.
func (r *releaser) startEnded(c *relCandidate, client, sdk string) error {
	if sdk != "0" {
		r.say("start completion of %s: not established (its manifest has no \"sdk\": \"0\")", c.Name)
		return nil
	}
	var last string
	for try := 0; try < 4; try++ {
		e, id, err := r.pluginClient(client, c.Name)
		if err != nil {
			return err
		}
		arg, _ := json.Marshal(map[string]any{"client_id": id, "action": "view.seen", "args": map[string]any{}, "timeout_ms": 30000})
		argv := []string{"rex", "-S", r.spec.Server, "--autostart=false", "--timeout", "40s", "api", "call", "client.perform", string(arg)}
		_, stderr, err := r.runCmd(relCmd{Argv: argv})
		if err == nil {
			return relFail("%s answered view.seen from the release tool: not the SDK 0 contract", c.Name)
		}
		m := rexFailure.FindStringSubmatch(strings.TrimSpace(string(stderr)))
		if m == nil {
			return relFail("view.seen to %s: %v: %s", c.Name, err, strings.TrimSpace(string(stderr)))
		}
		msg, code := m[1], m[2]
		if code != "refused" && code != "timeout" {
			return relFail("%s instance %s did not finish its start: view.seen got %s: %s", c.Name, e.InstanceID, code, msg)
		}
		if code == "refused" && msg != "Only the hub calls view.seen" {
			return relFail("view.seen to %s got the refusal %q, not the SDK 0 refusal", c.Name, msg)
		}
		e2, id2, err := r.pluginClient(client, c.Name)
		if err != nil {
			return relFail("%s instance %s ended during its start: %v", c.Name, e.InstanceID, err)
		}
		if e2.InstanceID != e.InstanceID || id2 != id {
			return relFail("%s instance %s ended during its start: now instance %s", c.Name, e.InstanceID, e2.InstanceID)
		}
		if code == "refused" {
			r.say("the start of %s instance %s has ended (view.seen refused by the SDK)", c.Name, e.InstanceID)
			return nil
		}
		last = msg
	}
	return relFail("the start of %s did not end in 4 tries of 30 s: %s", c.Name, last)
}

func relPluginPrecheck(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	if s, _ := r.stamp(); s != "" {
		return relFail("a backup stamp exists (%s): this run already passed step backup", s)
	}
	// The saved hub is the checkpoint of a passed precheck. A failed precheck, also a failed rerun,
	// leaves none, so pull and replace refuse (review R14).
	hubFile := filepath.Join(r.run, relPluginHubFile)
	if err := os.Remove(hubFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := r.holdOK(c); err != nil {
		return err
	}
	tool := ""
	if !r.spec.Lab {
		if tool, err = r.toolPath(); err != nil {
			return err
		}
		// A set step has no rehearsal record in its run: the set gate below reads the set record.
		if r.spec.Set == nil {
			if err := r.checkRehearsal(tool); err != nil {
				return err
			}
		}
	}
	panes, err := r.devPanes()
	if err != nil {
		return err
	}
	if len(panes) != 1 {
		return relFail("expected one %s pane, found %d", devPaneLabel, len(panes))
	}
	h, err := r.checkPluginHub(c, "precheck")
	if err != nil {
		return err
	}
	if r.spec.Set != nil {
		if err := r.checkSetGate(c, h, tool); err != nil {
			return err
		}
	}
	if err := r.verifyList(relVerifyBack); err != nil {
		return err
	}
	if err := r.startEnded(c, h.Client, c.FromSDK); err != nil {
		return err
	}
	if err := writeJSON(hubFile, h); err != nil {
		return err
	}
	r.say("OK: %s runs FROM %s, every pin and file as frozen, the frozen hub (pid %d), rehearsal record matches", c.Name, c.From.Commit, h.PID)
	return nil
}

// relPluginPull moves the pin of P to TO through plugin pull of the forward document, with the frozen
// hub binary. It never builds.
func relPluginPull(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	if err := r.holdBackup(c, "from from", "to to", "to from"); err != nil {
		return err
	}
	st, err := r.pinState(c)
	if err != nil {
		return err
	}
	bin := r.devHubPath()
	switch st {
	case "from from", "to from":
		if _, err := r.checkPluginHub(c, "forward"); err != nil {
			return err
		}
		outcome, local := "taken", c.From.Commit
		if st == "to from" {
			r.say("the lock is at TO and the base at FROM (a pull stopped after the lock write)")
			outcome, local = "unchanged", c.To.Commit
		}
		if err := r.mutate("pull-fwd", map[string]string{"doc": relFwdDoc}, func() (any, error) {
			return nil, r.pull(bin, relFwdDoc, pullLine(outcome, c, local, c.To.Commit))
		}); err != nil {
			return err
		}
	case "to to":
		r.say("the pin of %s is already at TO", c.Name)
	}
	if err := r.holdAfterPull(c); err != nil {
		return err
	}
	return r.statusOK(bin)
}

func relPluginReplace(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	if err := r.holdAfterPull(c); err != nil {
		return err
	}
	h, err := r.checkPluginHub(c, "forward")
	if err != nil {
		return err
	}
	return r.replacePlugin(c, h.Client, c.From, c.To)
}

func relPluginVerify(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	// The pins too: the lock and base of P at TO; every other pin and base, the config and the
	// managed file as in the backup (review R16). A restart must run what verify saw.
	if err := r.holdAfterPull(c); err != nil {
		return err
	}
	h, err := r.checkPluginHub(c, "forward")
	if err != nil {
		return err
	}
	if err := r.verifyList(relVerifyFwd); err != nil {
		return err
	}
	return r.startEnded(c, h.Client, c.ToSDK)
}

// relPluginRollback moves the pin of P back to FROM, and makes P run FROM. The runtime part reads
// hub.plugins once (plan c4 v2 section 5):
//
//	a. P runs FROM, registered by its instance: no replace (the hub checks the full run with
//	   expect_run, and changes nothing).
//	b. P runs TO with an instance, not pending: hub.admin.replace with the observed (or saved)
//	   instance and expect_run FROM.
//	c. P has no instance, desired running: hub.admin.start with expect_run FROM.
//	d. an unresolved spawn intent: refuse, no mutation.
//	e. P runs another commit: refuse, no mutation.
//
// Any other state (desired stopped: an external stop) is refused with no mutation.
func relPluginRollback(r *releaser) error {
	c, err := r.readCandidate()
	if err != nil {
		return err
	}
	b, err := r.currentBackup()
	if err != nil {
		return err
	}
	if err := r.checkBackup(b); err != nil {
		return err
	}
	if err := r.othersEqual(c, b); err != nil {
		return err
	}
	if _, err := r.checkPluginHub(c, "rollback"); err != nil {
		return err
	}
	st, err := r.pinState(c)
	if err != nil {
		return err
	}
	bin := r.devHubPath()
	pull := func(op, doc, outcome, local, peer string) error {
		if _, err := r.checkPluginHub(c, "rollback"); err != nil {
			return err
		}
		return r.mutate(op, map[string]string{"doc": doc}, func() (any, error) {
			return nil, r.pull(bin, doc, pullLine(outcome, c, local, peer))
		})
	}
	switch st {
	case "from from":
		r.say("the pin of %s is FROM; no pull", c.Name)
	case "to to":
		if err := pull("pull-back", relBackDoc, "taken", c.To.Commit, c.From.Commit); err != nil {
			return err
		}
	case "to from":
		r.say("the lock is at TO and the base at FROM (the forward pull stopped after the lock write)")
		if err := pull("pull-fwd", relFwdDoc, "unchanged", c.To.Commit, c.To.Commit); err != nil {
			return err
		}
		if err := pull("pull-back", relBackDoc, "taken", c.To.Commit, c.From.Commit); err != nil {
			return err
		}
	case "from to":
		r.say("the lock is at FROM and the base at TO (a rollback pull stopped after the lock write)")
		if err := pull("pull-back", relBackDoc, "unchanged", c.From.Commit, c.From.Commit); err != nil {
			return err
		}
	default:
		return relFail("the pin state of %s is %q: not a state of this release; stop and report", c.Name, st)
	}
	if st, err := r.pinState(c); err != nil || st != "from from" {
		return relFail("after the pull, the pin state of %s is %q (%v)", c.Name, st, err)
	}
	if err := r.othersEqual(c, b); err != nil {
		return err
	}
	h, err := r.checkPluginHub(c, "rollback")
	if err != nil {
		return err
	}
	if err := r.pluginBack(c, h.Client); err != nil {
		return err
	}
	if err := r.verifyList(relVerifyBack); err != nil {
		return err
	}
	if err := r.startEnded(c, h.Client, c.FromSDK); err != nil {
		return err
	}
	s, _ := r.stamp()
	if err := os.Rename(filepath.Join(r.run, relStampFile), filepath.Join(r.run, relStampFile+".rolled-back-"+s)); err != nil {
		return err
	}
	r.say("rolled back %s to FROM %s", c.Name, c.From.Commit)
	return nil
}

// pluginBack is the runtime part of relPluginRollback: rows a to e.
func (r *releaser) pluginBack(c *relCandidate, client string) error {
	entries, _, err := r.hubPlugins(client)
	if err != nil {
		return err
	}
	e := entryOf(entries, c.Name)
	switch {
	case e == nil:
		return relFail("hub.plugins has no entry %s; stop and report", c.Name)
	case e.Source != c.Source:
		return relFail("the %s entry has source %s, not %s; stop and report", c.Name, e.Source, c.Source)
	case e.InstancePending:
		return relFail("plugin %s has an unresolved spawn intent; do not retry: wait for the reconciliation, or report", c.Name)
	case e.Desired != "running":
		return relFail("plugin %s is desired %q (an external stop): not a state of this release; nothing is changed; stop and report", c.Name, e.Desired)
	case e.Commit != "" && e.Commit != c.From.Commit && e.Commit != c.To.Commit:
		return relFail("plugin %s runs %s, neither FROM nor TO; nothing is changed; stop and report", c.Name, e.Commit)
	}
	_, open, err := r.lastBegin("replace-" + c.From.Commit[:12])
	if err != nil {
		return err
	}
	if e.InstanceID == "" && !open {
		// Row c. A rerun sends the same expectation: the start either ran (row a then) or not.
		xrun := relExpectRun(c, c.From)
		r.say("%s has no instance and is desired running: start it with expect_run FROM", c.Name)
		return r.mutate("start-"+c.From.Commit[:12], relReplaceSaved{ExpectRun: xrun}, func() (any, error) {
			out, err := r.hubDo(client, "hub.admin.start", map[string]any{"plugin": c.Name, "expect_run": xrun})
			return json.RawMessage(nonEmptyJSON(out)), err
		})
	}
	// Rows a and b, and an interrupted replace of this rollback (its saved expectation).
	return r.replacePlugin(c, client, c.To, c.From)
}

// relKinds runs the step of the run's kind. A nil step of a kind is refused.
func relKinds(hub, plugin func(*releaser) error) func(*releaser) error {
	return func(r *releaser) error {
		switch {
		case r.spec.Kind == "hub" && hub != nil:
			return hub(r)
		case r.spec.Kind == "plugin" && plugin != nil:
			return plugin(r)
		}
		return relFail("step %s is not a step of a %s release", r.step, r.spec.Kind)
	}
}

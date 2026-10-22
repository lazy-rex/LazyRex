package main

// The dev pane and the running hub, as a release sees them (docs/releasing.md). The live hub runs
// dev.sh in an unplaced pane labeled lazyrex-dev-process. A release stops that pane, and starts a
// new one with the dev.sh of the checked-out commit. The hub writes hub_started to
// <state>/events.jsonl with its client and pid.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const devPaneLabel = "lazyrex-dev-process"

// hubStartEvent is one hub_started event: the hub client and its pid. Seq counts the hub_started
// events of the file, from 1.
type hubStartEvent struct {
	Client string `json:"client"`
	PID    int    `json:"pid"`
	Seq    int    `json:"seq"`
}

// hubEvents reads <state>/events.jsonl: the last hub_started event, and the plugin events after it.
// A line that is not JSON is skipped (a cut last line). A missing file has no start.
func hubEvents(state string) (last hubStartEvent, after []map[string]any, err error) {
	f, err := os.Open(filepath.Join(state, "events.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return hubStartEvent{}, nil, nil
	}
	if err != nil {
		return hubStartEvent{}, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev["event"] == "hub_started" {
			c, _ := ev["client_id"].(string)
			p, _ := ev["pid"].(float64)
			last = hubStartEvent{Client: c, PID: int(p), Seq: last.Seq + 1}
			after = nil
			continue
		}
		if _, ok := ev["plugin"]; ok {
			after = append(after, ev)
		}
	}
	return last, after, sc.Err()
}

// devPanes lists each lazyrex-dev-process pane as {session, block}. Any failed or malformed
// discovery read is an error: a failed read does not prove that no dev pane runs.
func (r *releaser) devPanes() ([][2]string, error) {
	out, err := r.rex("api", "call", "session.list", "{}")
	if err != nil {
		return nil, relFail("the pane discovery failed: %v", err)
	}
	var sl struct {
		Sessions []struct {
			ID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(out, &sl); err != nil || sl.Sessions == nil {
		return nil, relFail("session.list is not a session list: %s", out)
	}
	var panes [][2]string
	for _, s := range sl.Sessions {
		arg, _ := json.Marshal(map[string]string{"session_id": s.ID})
		out, err := r.rex("api", "call", "session.list_blocks", string(arg))
		if err != nil {
			return nil, relFail("the pane discovery failed: %v", err)
		}
		var bl struct {
			Blocks []struct {
				ID    string `json:"block_id"`
				Label string `json:"label"`
			} `json:"blocks"`
		}
		if err := json.Unmarshal(out, &bl); err != nil || bl.Blocks == nil {
			return nil, relFail("session.list_blocks %s is not a block list: %s", s.ID, out)
		}
		for _, b := range bl.Blocks {
			if b.Label == devPaneLabel {
				panes = append(panes, [2]string{s.ID, b.ID})
			}
		}
	}
	return panes, nil
}

// procAlive and procCommand are the process reads of a release; tests replace them.
func (r *releaser) procAlive(pid int, start string) bool {
	if r.alive != nil {
		return r.alive(pid, start)
	}
	return alive(pid, start)
}

func (r *releaser) procStart(pid int) string {
	if r.startOf != nil {
		return r.startOf(pid)
	}
	return startTime(pid)
}

func (r *releaser) procExe(pid int) string {
	if r.exeOf != nil {
		return r.exeOf(pid)
	}
	argv, err := procArgv(pid)
	if err != nil || len(argv) == 0 {
		return ""
	}
	return argv[0]
}

func (r *releaser) pause(d time.Duration) {
	if r.sleep != nil {
		r.sleep(d)
		return
	}
	time.Sleep(d)
}

// relHub is a hub process that a step recorded: the pid, its start time and its client.
type relHub struct {
	PID    int    `json:"pid"`
	Start  string `json:"start"`
	Client string `json:"client"`
	Seq    int    `json:"seq"`
}

// running reports whether the recorded hub still runs: the same pid and start time, and it is
// the hub of the last hub_started event.
func (r *releaser) running(h *relHub) bool {
	if h == nil || h.PID == 0 {
		return false
	}
	last, _, err := hubEvents(r.cfg.state)
	return err == nil && last.PID == h.PID && last.Client == h.Client && r.procAlive(h.PID, h.Start)
}

// currentHub records the hub of the last hub_started event, when its process runs.
func (r *releaser) currentHub() (*relHub, error) {
	last, _, err := hubEvents(r.cfg.state)
	if err != nil {
		return nil, err
	}
	if last.PID == 0 {
		return nil, relFail("no hub_started event in %s", filepath.Join(r.cfg.state, "events.jsonl"))
	}
	st := r.procStart(last.PID)
	if st == "" {
		return nil, relFail("the hub pid %d of the last hub_started event does not run", last.PID)
	}
	return &relHub{PID: last.PID, Start: st, Client: last.Client, Seq: last.Seq}, nil
}

// devStop closes the one dev pane and waits until its hub process ends (15 s). It writes the
// pane's session to <run>/session for devStart. With needHub false, a pane whose hub does not run
// (a failed build or start, or an ended hub) is closed with no wait.
func (r *releaser) devStop(needHub bool) error {
	panes, err := r.devPanes()
	if err != nil {
		return err
	}
	if len(panes) != 1 {
		return relFail("expected one %s pane, found %d", devPaneLabel, len(panes))
	}
	h, err := r.currentHub()
	if err != nil && needHub {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.run, "session"), []byte(panes[0][0]+"\n"), 0o600); err != nil {
		return err
	}
	arg, _ := json.Marshal(map[string]string{"session_id": panes[0][0], "block_id": panes[0][1]})
	if h == nil {
		r.say("closing the dev pane %s in %s (no hub runs: %v)", panes[0][1], panes[0][0], err)
		_, err := r.rex("api", "call", "block.close", string(arg))
		return err
	}
	r.say("closing the dev pane %s in %s (hub pid %d)", panes[0][1], panes[0][0], h.PID)
	if _, err := r.rex("api", "call", "block.close", string(arg)); err != nil {
		return err
	}
	for i := 0; i < 15 && r.procAlive(h.PID, h.Start); i++ {
		r.pause(time.Second)
	}
	if r.procAlive(h.PID, h.Start) {
		return relFail("the hub pid %d still runs 15 s after the dev pane closed", h.PID)
	}
	return nil
}

// goBinDir is the directory of the go command on the child PATH. dev.sh builds the hub, and a
// login zsh does not always have go on its PATH.
func (r *releaser) goBinDir() (string, error) {
	if d := r.pathDir("go"); d != "" {
		return d, nil
	}
	return "", relFail("no go command on PATH")
}

// pathDir is the first directory of the child PATH that has an executable file name, "" if none.
func (r *releaser) pathDir(name string) string {
	for _, kv := range r.env {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok {
			for _, d := range filepath.SplitList(p) {
				if st, err := os.Stat(filepath.Join(d, name)); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
					return d
				}
			}
		}
	}
	return ""
}

// devStart starts a dev pane that runs devsh in the session of <run>/session, and waits up to 180 s
// for a new hub_started event whose process runs. dev.sh builds the hub first.
func (r *releaser) devStart(devsh string) (*relHub, error) {
	sb, err := os.ReadFile(filepath.Join(r.run, "session"))
	if err != nil || strings.TrimSpace(string(sb)) == "" {
		return nil, relFail("no session of the dev pane in %s (the stop step writes it)", filepath.Join(r.run, "session"))
	}
	gobin, err := r.goBinDir()
	if err != nil {
		return nil, err
	}
	before, _, err := hubEvents(r.cfg.state)
	if err != nil {
		return nil, err
	}
	real := ""
	if r.spec.Real {
		real = "DEV_REAL=1 "
	}
	cmd := fmt.Sprintf("export PATH=%s:$PATH; %sexec %s %s 2>&1 | tee -a %s", shQuote(gobin), real, shQuote(devsh), shQuote(r.cfg.path), shQuote(r.spec.DevLog))
	arg, _ := json.Marshal(map[string]any{"session_id": strings.TrimSpace(string(sb)), "flavor": "com.superlogical.terminal.shell", "label": devPaneLabel,
		"options": map[string]any{"command": []string{"/bin/zsh", "-lc", cmd}, "login": false, "exit": map[string]bool{"on_completion": false}}})
	if _, err := r.rex("api", "call", "session.new_block", string(arg)); err != nil {
		return nil, err
	}
	r.say("the dev pane runs %s; waiting for its hub", devsh)
	for i := 0; i < 90; i++ {
		r.pause(2 * time.Second)
		last, _, err := hubEvents(r.cfg.state)
		if err != nil || last.Seq <= before.Seq {
			continue
		}
		if st := r.procStart(last.PID); st != "" {
			return &relHub{PID: last.PID, Start: st, Client: last.Client, Seq: last.Seq}, nil
		}
	}
	return nil, relFail("no new hub after 180 s; see %s", r.spec.DevLog)
}

// shQuote quotes a word for sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// relEntryState is one hub.plugins entry, as a release reads it.
type relEntryState struct {
	Name            string `json:"name"`
	Source          string `json:"source"`
	Commit          string `json:"commit"`
	State           string `json:"state"`
	Registered      *bool  `json:"registered"`
	InstanceID      string `json:"instance_id"`
	InstancePending bool   `json:"instance_pending"`
	Desired         string `json:"desired"`
}

// hubPlugins calls hub.plugins on the hub client.
func (r *releaser) hubPlugins(client string) ([]relEntryState, []string, error) {
	out, err := r.hubDo(client, "hub.plugins", map[string]any{})
	if err != nil {
		return nil, nil, err
	}
	var list struct {
		Entries []relEntryState `json:"entries"`
		Kept    []struct {
			Name string `json:"name"`
		} `json:"kept"`
	}
	if err := json.Unmarshal(out, &list); err != nil || list.Entries == nil {
		return nil, nil, relFail("hub.plugins is not a plugin list: %s", out)
	}
	var kept []string
	for _, k := range list.Kept {
		kept = append(kept, k.Name)
	}
	return list.Entries, kept, nil
}

// entryOf returns the hub.plugins entry of a plugin name.
func entryOf(entries []relEntryState, name string) *relEntryState {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}

// relVerifyEntry is one expected entry of a verify list.
type relVerifyEntry struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Commit string `json:"commit"`
}

// registeredNow reports whether the entry runs and is registered by its current instance, after
// the last hub_started (the rule of the release kits' verify.sh): a plugin_registered event names
// the instance, and no plugin_stopped or plugin_spawned event of the plugin comes after it.
func registeredNow(e relEntryState, after []map[string]any) bool {
	if e.State != "running" || e.InstanceID == "" || e.InstancePending || e.Registered == nil || !*e.Registered {
		return false
	}
	seen := false
	for _, ev := range after {
		if ev["plugin"] != e.Name {
			continue
		}
		switch ev["event"] {
		case "plugin_registered":
			if ev["instance_id"] == e.InstanceID {
				seen = true
			}
		case "plugin_stopped", "plugin_spawned":
			seen = false
		}
	}
	return seen
}

// verifyList checks, until a 120 s deadline, that hub.plugins of the current hub has exactly the
// expected entries, each running and registered by its current instance, and no kept record.
func (r *releaser) verifyList(file string) error {
	b, err := os.ReadFile(filepath.Join(r.run, file))
	if err != nil {
		return relFail("the verify list: %v", err)
	}
	var want []relVerifyEntry
	if err := decodeStrict(b, &want); err != nil || len(want) == 0 {
		return relFail("%s: not a verify list: %v", file, err)
	}
	sort.Slice(want, func(i, j int) bool { return want[i].Name < want[j].Name })
	var last string
	for i := 0; i < 60; i++ {
		if i > 0 {
			r.pause(2 * time.Second)
		}
		start, after, err := hubEvents(r.cfg.state)
		if err != nil || start.Client == "" {
			last = fmt.Sprintf("no hub_started event (%v)", err)
			continue
		}
		entries, kept, err := r.hubPlugins(start.Client)
		if err != nil {
			last = err.Error()
			continue
		}
		var got []relVerifyEntry
		var notReady []string
		for _, e := range entries {
			got = append(got, relVerifyEntry{Name: e.Name, Source: e.Source, Commit: e.Commit})
			if !registeredNow(e, after) {
				notReady = append(notReady, e.Name)
			}
		}
		sort.Slice(got, func(i, j int) bool { return got[i].Name < got[j].Name })
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		switch {
		case !bytes.Equal(gb, wb):
			last = fmt.Sprintf("entries %s, want %s", gb, wb)
		case len(notReady) > 0:
			last = fmt.Sprintf("not running and registered by the current instance: %s", strings.Join(notReady, ", "))
		case len(kept) > 0:
			last = fmt.Sprintf("kept records: %s", strings.Join(kept, ", "))
		default:
			r.say("OK: %d entries running and registered by their current instance, no kept record", len(want))
			return nil
		}
	}
	return relFail("verify %s at the 120 s deadline: %s", file, last)
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFormInPlaceLab (lead-bug-404): a form that opens the next form from its action keeps its
// pane. With the real hub and renderer, the forms a -> b -> c of testdata/inplace run in one pane
// and one renderer process: no pane is created or closed, the pane label names the current view,
// the current view is the only open form and is attached in that pane, and the hub recorded each
// replacement in place (becoming, then done). The Rex app's dialog ("block ... not found in any
// window") came from the pane close that this removes; a lab has no app, so the dialog itself is
// checked on the live hub. Split and layer slot modes. Lab only: LAZYREX_LAB=1.
func TestFormInPlaceLab(t *testing.T) {
	for _, slot := range []string{"split", "layer"} {
		t.Run(slot, func(t *testing.T) {
			l := newChunkLab(t, "inplace", "view/testdata/inplace")
			if slot == "layer" {
				l.setHubConfig("ui_slot", "layer")
			}
			pane := l.open("lazyrex_inplace_a")
			l.waitFor("form a", 30*time.Second, func() bool { return strings.Contains(l.screen(pane), "LAB-FORM-A") })
			pid := l.paneChild(pane)
			before := l.blockLabels()
			for _, letter := range []string{"b", "c"} {
				l.key(pane, "enter")
				want := "LAB-FORM-" + strings.ToUpper(letter)
				l.waitFor("form "+letter+" in the same pane", 20*time.Second, func() bool { return strings.Contains(l.screen(pane), want) })
				id := l.onlyForm("/forms/inplace/"+letter, pane)
				l.waitFor("the replacement of form "+letter+" done", 20*time.Second, func() bool { return l.slotDone(id) })
				labels := l.blockLabels()
				if len(labels) != len(before) {
					t.Fatalf("form %s: the session has %d blocks, had %d: %v", letter, len(labels), len(before), labels)
				}
				if labels[pane] != "lazyrex-form:"+id {
					t.Fatalf("form %s: the pane label is %q, want lazyrex-form:%s", letter, labels[pane], id)
				}
				if got := l.paneChild(pane); got != pid {
					t.Fatalf("form %s: the pane runs pid %d, want the same renderer process %d", letter, got, pid)
				}
			}
		})
	}
}

// setHubConfig sets one key of the lab hub's config and restarts the hub.
func (l *chunkLab) setHubConfig(key string, value any) {
	l.t.Helper()
	f := filepath.Join(l.lab, "hub.json")
	var c map[string]any
	if err := json.Unmarshal(must(os.ReadFile(f)), &c); err != nil {
		l.t.Fatal(err)
	}
	c[key] = value
	b, _ := json.Marshal(c)
	os.WriteFile(f, b, 0o600)
	l.stopHub()
	l.startHub()
	l.waitFor("the restarted lab hub", 30*time.Second, func() bool { return l.hubClient() != "" })
}

// paneChild is the pid of the process that runs in pane.
func (l *chunkLab) paneChild(pane string) int {
	l.t.Helper()
	var p struct {
		Child struct {
			PID int `json:"pid"`
		} `json:"child"`
	}
	out := l.must("block", "call", "-s", l.session, "-b", pane, "process")
	if err := json.Unmarshal([]byte(out), &p); err != nil || p.Child.PID == 0 {
		l.t.Fatalf("the process of pane %s: %q (%v)", pane, out, err)
	}
	return p.Child.PID
}

// blockLabels maps each block of the lab session to its label.
func (l *chunkLab) blockLabels() map[string]string {
	l.t.Helper()
	var r struct {
		Blocks []struct {
			ID    string `json:"block_id"`
			Label string `json:"label"`
		} `json:"blocks"`
	}
	json.Unmarshal([]byte(l.must("block", "ls", "-s", l.session, "--json")), &r)
	out := map[string]string{}
	for _, b := range r.Blocks {
		out[b.ID] = b.Label
	}
	return out
}

// onlyForm checks that the plugin's one open view is the form route, attached in pane, and
// returns its view ID. It waits for the attach.
func (l *chunkLab) onlyForm(route, pane string) string {
	l.t.Helper()
	var id string
	var last any
	l.waitFor("the only open form "+route+", attached in the pane", 20*time.Second, func() bool {
		var out struct {
			Views []struct {
				ID       string `json:"view_id"`
				Route    string `json:"route"`
				Attached bool   `json:"attached"`
				Block    string `json:"block_id"`
			} `json:"views"`
		}
		l.hubTest("hub.test.views", map[string]any{}, &out)
		last = out
		if len(out.Views) != 1 || out.Views[0].Route != route || !out.Views[0].Attached || out.Views[0].Block != pane {
			return false
		}
		id = out.Views[0].ID
		return true
	})
	if id == "" {
		l.t.Fatalf("views: %v", last)
	}
	return id
}

// slotDone reports whether the hub's events show the in-place replacement that opened view as
// becoming and then done.
func (l *chunkLab) slotDone(view string) bool {
	var repl string
	becoming, done := false, false
	for _, line := range strings.Split(string(must(os.ReadFile(filepath.Join(l.lab, "state-hub", "events.jsonl")))), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e["event"] == "slot_replacement" && e["new_view"] == view {
			repl, _ = e["replacement_id"].(string)
		}
		if e["event"] == "slot_state" && repl != "" && e["replacement_id"] == repl {
			switch e["state"] {
			case "becoming":
				becoming = e["in_place"] == true
			case "done":
				done = becoming
			}
		}
	}
	return done
}

// TestFormInPlaceLostBecomeLab: the hub's view.become is lost twice, in two ways (the test fault
// view-become). a -> b: the first request is not sent (drop); the worker sends it again after its
// backoff, and the transfer ends. b -> c: the request reaches the renderer and its answer is lost
// (lose-reply); the attach of c is the proof, and the transfer ends with no second request. Each
// time the form stays in its one pane and process. Lab only: LAZYREX_LAB=1.
func TestFormInPlaceLostBecomeLab(t *testing.T) {
	l := newChunkLab(t, "inplace", "view/testdata/inplace")
	pane := l.open("lazyrex_inplace_a")
	l.waitFor("form a", 30*time.Second, func() bool { return strings.Contains(l.screen(pane), "LAB-FORM-A") })
	pid := l.paneChild(pane)
	before := len(l.blockLabels())
	for _, step := range []struct{ letter, mode string }{{"b", "drop"}, {"c", "lose-reply"}} {
		var armed map[string]any
		l.hubTest("hub.test.fault", map[string]any{"point": "view-become", "mode": step.mode, "key": "/forms/inplace/" + step.letter}, &armed)
		l.key(pane, "enter")
		want := "LAB-FORM-" + strings.ToUpper(step.letter)
		l.waitFor("form "+step.letter+" in the same pane", 30*time.Second, func() bool { return strings.Contains(l.screen(pane), want) })
		id := l.onlyForm("/forms/inplace/"+step.letter, pane)
		l.waitFor("the replacement of form "+step.letter+" done", 30*time.Second, func() bool { return l.slotDone(id) })
		sent := l.becomeErrors(id)
		switch step.mode {
		case "drop":
			if len(sent) < 2 || !strings.Contains(sent[0], "drop") || sent[len(sent)-1] != "" {
				t.Fatalf("form b: view.become results %q, want the dropped one, then a sent one", sent)
			}
		case "lose-reply":
			if len(sent) != 1 || !strings.Contains(sent[0], "lose-reply") {
				t.Fatalf("form c: view.become results %q, want one lost answer", sent)
			}
		}
		if n := len(l.blockLabels()); n != before {
			t.Fatalf("form %s: %d blocks, want %d", step.letter, n, before)
		}
		if got := l.paneChild(pane); got != pid {
			t.Fatalf("form %s: pid %d, want %d", step.letter, got, pid)
		}
	}
}

// TestFormInPlaceHubRestartLab: the hub stops (SIGKILL) while a transfer is becoming: the
// view.become request is held (the test fault view-become hold), so the renderer still runs form
// a. The restarted hub finishes the row: the renderer's attach of a gets the current view b
// (replaced), or the new hub's worker asks it; b runs in the same pane and process, labeled for
// b, the only open form. Lab only: LAZYREX_LAB=1.
func TestFormInPlaceHubRestartLab(t *testing.T) {
	l := newChunkLab(t, "inplace", "view/testdata/inplace")
	pane := l.open("lazyrex_inplace_a")
	l.waitFor("form a", 30*time.Second, func() bool { return strings.Contains(l.screen(pane), "LAB-FORM-A") })
	pid := l.paneChild(pane)
	before := len(l.blockLabels())
	var armed map[string]any
	l.hubTest("hub.test.fault", map[string]any{"point": "view-become", "mode": "hold", "key": "/forms/inplace/b", "hold_ms": float64(30000)}, &armed)
	l.key(pane, "enter")
	l.waitFor("a becoming row", 20*time.Second, func() bool { return l.eventCount("slot_state", "state", "becoming") == 1 })
	l.hub.Process.Kill()
	l.hub.Wait()
	l.startHub()
	l.waitFor("form b in the same pane", 60*time.Second, func() bool { return strings.Contains(l.screen(pane), "LAB-FORM-B") })
	id := l.onlyForm("/forms/inplace/b", pane)
	l.waitFor("the replacement of form b done", 60*time.Second, func() bool { return l.slotDone(id) })
	labels := l.blockLabels()
	if len(labels) != before || labels[pane] != "lazyrex-form:"+id {
		t.Fatalf("blocks %v, want %d with the pane labeled for %s", labels, before, id)
	}
	if got := l.paneChild(pane); got != pid {
		t.Fatalf("pid %d, want %d", got, pid)
	}
}

// becomeErrors lists the error of each slot_become event of the transfer to view, in order ("" for
// a request that Rex answered).
func (l *chunkLab) becomeErrors(view string) []string {
	var out []string
	for _, e := range l.hubEvents() {
		if e["event"] == "slot_become" && e["view_id"] == view {
			s, _ := e["error"].(string)
			out = append(out, s)
		}
	}
	return out
}

// eventCount counts the hub events named name whose field key is value.
func (l *chunkLab) eventCount(name, key string, value any) int {
	n := 0
	for _, e := range l.hubEvents() {
		if e["event"] == name && e[key] == value {
			n++
		}
	}
	return n
}

func (l *chunkLab) hubEvents() []map[string]any {
	var out []map[string]any
	b, _ := os.ReadFile(filepath.Join(l.lab, "state-hub", "events.jsonl"))
	for _, line := range strings.Split(string(b), "\n") {
		var e map[string]any
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

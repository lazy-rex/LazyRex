package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestBackgroundOpenProbeLab (coord-tvglitch-2, a probe): the stage 2 thread view opens in a pane
// that no app shows and that nobody resizes, as a background open on the live hub does. The probe
// logs the pane's size and capture before any resize, then after the sizes an app gives when it
// shows the pane. Lab only: BGOPEN_LAB=1 with MODE_LAB=1; VIEW_TEA_TRACE=1 keeps the writes.
func TestBackgroundOpenProbeLab(t *testing.T) {
	if os.Getenv("BGOPEN_LAB") != "1" {
		t.Skip("set BGOPEN_LAB=1 to run the background open probe")
	}
	l := newModeLab(t)
	l.sql("UPDATE stage SET n = 2", true)
	l.waitFor("the palette entry", 30*time.Second, func() bool {
		out, _ := l.cli("actions")
		return strings.Contains(out, "lazyrex_modelab_mode")
	})
	blocks := func() string { return l.must("api", "call", "session.list_blocks", `{"session_id":"`+l.session+`"}`) }
	before := blocks()
	l.cli("-s", l.session, "do", "lazyrex_modelab_mode")
	var pane string
	l.waitFor("the renderer pane", 20*time.Second, func() bool {
		var r struct {
			Blocks []struct {
				ID string `json:"block_id"`
			} `json:"blocks"`
		}
		json.Unmarshal([]byte(blocks()), &r)
		for _, b := range r.Blocks {
			if !strings.Contains(before, b.ID) {
				pane = b.ID
			}
		}
		return pane != ""
	})
	time.Sleep(3 * time.Second) // the renderer draws at the size Rex gave the pane
	t.Logf("blocks before any resize:\n%s", blocks())
	t.Logf("capture before any resize:\n%s", l.screen(pane))
	if err := l.owner.call(l.ctx, "session.own_block", map[string]any{"session_id": l.session, "block_ids": []string{pane}, "mode": "take"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]int{{80, 24}, {48, 23}, {80, 24}} {
		l.resize(pane, size[0], size[1])
		time.Sleep(2 * time.Second)
		last := l.screen(pane)
		t.Logf("capture at %dx%d:\n%s", size[0], size[1], last)
		for _, text := range []string{"╭", "DRAFT-TEXT", "Ctrl+Q quit"} {
			if n := strings.Count(last, text); n != 1 {
				t.Errorf("%dx%d: %d copies of %q, want 1", size[0], size[1], n, text)
			}
		}
	}
}

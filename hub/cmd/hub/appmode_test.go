package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCheckViewAcceptsProvenLayouts(t *testing.T) {
	raw := `{"revision":5,"session_id":"s","label":"x","active_window_id":"w1","windows":[
	 {"window_id":"w1","label":"one","focused_block_id":"a","zoomed_block_id":"a","layers":[
	  {"layer_id":"l1","kind":"tiled","bounds":[0,0,1,1],"layout":{"rect":[0,0,1,1],"split":{"split_id":"sp","direction":"horizontal","ratio":0.3,
	   "before":{"rect":[0,0,0.3,1],"block_id":"a"},"after":{"rect":[0.3,0,0.7,1],"block_id":"b"}}},"blocks":[]},
	  {"layer_id":"l2","kind":"floating","bounds":[0.1,0.1,0.5,0.5],"layout":{"rect":[0.1,0.1,0.5,0.5],"block_id":"c"},"blocks":[]}]},
	 {"window_id":"w2","label":"floats only","layers":[{"layer_id":"l3","kind":"tiled","bounds":[0,0,1,1],"blocks":[]},
	  {"layer_id":"l4","kind":"floating","bounds":[0.2,0.2,0.6,0.6],"layout":{"block_id":"d"},"blocks":[]}]}]}`
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if msg := checkView(m); msg != "" {
		t.Fatalf("refused a proven layout: %s", msg)
	}
}

func TestCheckViewRefusesUnknownFeatures(t *testing.T) {
	for name, raw := range map[string]string{
		"window field": `{"windows":[{"window_id":"w","layers":[],"pinned":true}]}`,
		"layer kind":   `{"windows":[{"window_id":"w","layers":[{"kind":"tiled","bounds":[0,0,1,1]},{"kind":"overlay","layout":{"block_id":"a"}}]}]}`,
		"first layer":  `{"windows":[{"window_id":"w","layers":[{"kind":"floating","bounds":[0,0,1,1],"layout":{"block_id":"a"}}]}]}`,
		"split field":  `{"windows":[{"window_id":"w","layers":[{"kind":"tiled","layout":{"split":{"direction":"horizontal","ratio":0.5,"weights":[1],"before":{"block_id":"a"},"after":{"block_id":"b"}}}}]}]}`,
		"direction":    `{"windows":[{"window_id":"w","layers":[{"kind":"tiled","layout":{"split":{"direction":"diagonal","ratio":0.5,"before":{"block_id":"a"},"after":{"block_id":"b"}}}}]}]}`,
		"ratio":        `{"windows":[{"window_id":"w","layers":[{"kind":"tiled","layout":{"split":{"direction":"vertical","ratio":1,"before":{"block_id":"a"},"after":{"block_id":"b"}}}}]}]}`,
		"session":      `{"windows":[],"tabs_pinned":1}`,
	} {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(name, err)
		}
		if checkView(m) == "" {
			t.Errorf("%s: accepted an unproven layout", name)
		}
	}
}

func TestPruneCollapsesSplits(t *testing.T) {
	var n lnode
	json.Unmarshal([]byte(`{"split":{"direction":"horizontal","ratio":0.3,"before":{"block_id":"a"},
	 "after":{"split":{"direction":"vertical","ratio":0.6,"before":{"block_id":"b"},"after":{"block_id":"c"}}}}}`), &n)
	got := n.prune(map[string]bool{"a": true, "c": true})
	want := &lnode{Split: &lsplit{Direction: "horizontal", Ratio: 0.3, Before: &lnode{BlockID: "a"}, After: &lnode{BlockID: "c"}}}
	if !reflect.DeepEqual(got, want) {
		b, _ := json.Marshal(got)
		t.Fatalf("prune: %s", b)
	}
	if n.prune(map[string]bool{}) != nil {
		t.Fatal("an empty keep set must prune everything")
	}
	if l := strings.Join(n.leaves(nil), ","); l != "a,b,c" {
		t.Fatalf("leaves: %s", l)
	}
}

// Every plugin shares the LazyRex session (one tab): a view in an older build's app session is
// legacy and retires.
func TestViewOutsideTheLazyRexSessionIsLegacy(t *testing.T) {
	h, _ := workerTestHub(t)
	h.ui = &uiState{sess: map[string]string{"home": uiSessionKey, "msgs": appKeyPrefix + "messaging"}}
	if h.legacyView("messaging", "home") || h.legacyView("lazyrex", "home") {
		t.Error("a view in the LazyRex session is current")
	}
	if !h.legacyView("messaging", "msgs") {
		t.Error("a view in an old app session must retire")
	}
}

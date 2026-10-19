package main

import "testing"

func TestParseTabStatus(t *testing.T) {
	ok := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{map[string]any{"state": "done", "msg": " 3 unread "}, `{"msg":"3 unread","state":"done"}`},
		{map[string]any{"state": "blocked", "kind": "question"}, `{"kind":"question","state":"blocked"}`},
		{map[string]any{"state": "working", "kind": nil, "msg": ""}, `{"state":"working"}`},
	}
	for _, c := range ok {
		if got, err := parseTabStatus(c.in); err != nil || got != c.want {
			t.Errorf("%v: %q %v, want %q", c.in, got, err, c.want)
		}
	}
	for _, in := range []any{
		"done",
		map[string]any{"state": "busy"},
		map[string]any{"state": "done", "kind": "question"},
		map[string]any{"state": "blocked", "kind": "other"},
		map[string]any{"state": "done", "msg": "two\nlines"},
		map[string]any{"state": "done", "progress": 50},
	} {
		if _, err := parseTabStatus(in); err == nil {
			t.Errorf("%v: accepted", in)
		}
	}
}

// A rendered tree.status leaves the tree; an invalid one is dropped so the declared one applies.
func TestLiftTreeStatus(t *testing.T) {
	tree := map[string]any{"title": "x", "status": map[string]any{"state": "error", "msg": "refresh failed"}}
	if got := liftTreeStatus(tree); got != `{"msg":"refresh failed","state":"error"}` {
		t.Fatalf("lifted %q", got)
	}
	if _, ok := tree["status"]; ok {
		t.Fatal("tree.status must leave the tree")
	}
	tree = map[string]any{"status": map[string]any{"state": "bad"}}
	if got := liftTreeStatus(tree); got != "" || tree["status"] != nil {
		t.Fatalf("invalid status: %q %v", got, tree)
	}
}

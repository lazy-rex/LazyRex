package main

import (
	"context"
	"strings"
	"testing"
)

// A source pane that reports no folder: the form says so in the user's words, without the
// protocol name (the onboarding dry run, 2026-10-22). A read error keeps its detail.
func TestFreezeSourcePwd(t *testing.T) {
	cases := []struct {
		name    string
		payload any
		fail    string
		pwd     string
		want    string
	}{
		{"folder", map[string]any{"pwd": "file:///Users/u/src"}, "", "file:///Users/u/src", ""},
		{"no folder", map[string]any{}, "", "", "This pane did not report its folder. Enter a directory, or leave it empty for each profile's own directory."},
		{"read error", nil, "not_found", "", "the source pane's directory could not be read: "},
	}
	for _, c := range cases {
		h := &Hub{}
		h.rex = replyRex(t, func(method string) (any, string) {
			if method != "com.superlogical.terminal.pwd" {
				t.Errorf("%s: method %s", c.name, method)
			}
			return c.payload, c.fail
		})
		fc := &formContext{SessionID: "session:1", SourceBlock: "block:1"}
		h.freezeSourcePwd(context.Background(), fc)
		if fc.SourcePwd != c.pwd {
			t.Errorf("%s: pwd %q, want %q", c.name, fc.SourcePwd, c.pwd)
		}
		if c.fail != "" {
			if !strings.HasPrefix(fc.SourcePwdError, c.want) {
				t.Errorf("%s: error %q, want the prefix %q", c.name, fc.SourcePwdError, c.want)
			}
		} else if fc.SourcePwdError != c.want {
			t.Errorf("%s: error %q, want %q", c.name, fc.SourcePwdError, c.want)
		}
		if strings.Contains(fc.SourcePwdError, "OSC") {
			t.Errorf("%s: the user text names the protocol: %q", c.name, fc.SourcePwdError)
		}
	}
}

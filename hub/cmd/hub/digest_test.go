package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// hub.digest: the callers, the input rules and the limits, and the digests.
func TestDigest(t *testing.T) {
	h, _ := workerTestHub(t)
	pid := os.Getpid()
	e := &configEntry{Source: "/s/p", name: "p"}
	h.plugins = map[string]*Plugin{
		"p":    {Name: "p", entry: e, ClientID: "c1", ChildPID: pid, ChildStart: startTime(pid)},
		"kept": {Name: "kept", ClientID: "c2", ChildPID: pid, ChildStart: startTime(pid)},
		"ret":  {Name: "ret", entry: &configEntry{name: "ret"}, state: "retained", ClientID: "c3", ChildPID: pid, ChildStart: startTime(pid)},
	}
	call := func(client, args string) (string, error) {
		var a map[string]any
		json.Unmarshal([]byte(args), &a)
		out, err := h.digest(client, a)
		b, _ := json.Marshal(out)
		return string(b), err
	}
	for _, c := range []string{"", "c2", "c3", "other"} {
		if _, err := call(c, `{"texts":["a"]}`); !digestRefused(err, "forbidden") {
			t.Errorf("caller %q: %v", c, err)
		}
	}
	out, err := call("c1", `{"texts":["", "abc"]}`)
	want := `{"digests":["e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"]}`
	if err != nil || out != want {
		t.Errorf("digests %s, %v", out, err)
	}
	big := strings.Repeat("x", digestMaxText)
	sixteen := `["` + strings.Repeat(`a","`, 15) + `a"]`
	if _, err := call("c1", `{"texts":`+sixteen+`}`); err != nil {
		t.Errorf("16 texts: %v", err)
	}
	if _, err := call("c1", `{"texts":["`+big+`","`+big+`","`+big+`","`+big+`"]}`); err != nil {
		t.Errorf("4 texts of the limit (the total limit exactly): %v", err)
	}
	bad := map[string]string{
		`{}`:                    "invalid_input",
		`{"texts":[]}`:          "invalid_input",
		`{"texts":"a"}`:         "invalid_input",
		`{"texts":[1]}`:         "invalid_input",
		`{"texts":[null]}`:      "invalid_input",
		`{"texts":["a"],"x":1}`: "invalid_input",
		`{"texts":` + sixteen[:len(sixteen)-1] + `,"a"]}`:                         "invalid_input",
		`{"texts":["` + big + `x"]}`:                                              "too_large",
		`{"texts":["` + big + `","` + big + `","` + big + `","` + big + `","y"]}`: "too_large",
	}
	for args, code := range bad {
		if _, err := call("c1", args); !digestRefused(err, code) {
			short := args
			if len(short) > 60 {
				short = short[:60]
			}
			t.Errorf("%s: %v, want code %s", short, err, code)
		}
	}
	if _, err := call("c1", `{"texts":["`+big+`x"]}`); err == nil || !strings.Contains(err.Error(), "texts[1] is 65537 bytes, over the 65536-byte limit") {
		t.Errorf("one text over: %v", err)
	}
	if _, err := call("c1", `{"texts":["`+big+`","`+big+`","`+big+`","`+big+`","y"]}`); err == nil || !strings.Contains(err.Error(), "the texts are 262145 bytes, over the 262144-byte limit") {
		t.Errorf("the total over: %v", err)
	}
}

// digestRefused reports a hub refusal with this code.
func digestRefused(err error, code string) bool {
	e, ok := err.(*storeError)
	return ok && e.code == code
}

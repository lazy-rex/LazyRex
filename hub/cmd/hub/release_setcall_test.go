package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReleaseSetExpects(t *testing.T) {
	res := []byte(`{"items":[{"name":"a<b>&c","n":1.50}],"pending":{},"list":[],"a/b":{"~x":true},"z":null}`)
	for name, tc := range map[string]struct {
		x    string // the expects as JSON
		want string // "" passes
	}{
		"value":          {`[{"pointer":"/items/0/name","value":"a<b>&c"}]`, ""},
		"number text":    {`[{"pointer":"/items/0/n","value":1.50}]`, ""},
		"number differs": {`[{"pointer":"/items/0/n","value":1.5}]`, "not 1.5"},
		"whole":          {`[{"pointer":"/items/0","value":{"n":1.50,"name":"a<b>&c"}}]`, ""},
		"empty object":   {`[{"pointer":"/pending","empty":true}]`, ""},
		"empty array":    {`[{"pointer":"/list","empty":true}]`, ""},
		"not empty":      {`[{"pointer":"/items","empty":true}]`, "not empty"},
		"null":           {`[{"pointer":"/z","value":null}]`, ""},
		"null empty":     {`[{"pointer":"/z","empty":true}]`, "not empty"},
		"escaped":        {`[{"pointer":"/a~1b/~0x","value":true}]`, ""},
		"no member":      {`[{"pointer":"/nope","value":1}]`, "no member"},
		"bad index":      {`[{"pointer":"/items/01","value":1}]`, "no element"},
		"past end":       {`[{"pointer":"/items/1","value":1}]`, "no element"},
		"below scalar":   {`[{"pointer":"/z/x","value":1}]`, "not an object or an array"},
		"wrong value":    {`[{"pointer":"/items/0/name","value":"x"}]`, `not "x"`},
	} {
		var xs []relSetExpect
		if err := json.Unmarshal([]byte(tc.x), &xs); err != nil {
			t.Fatal(err)
		}
		err := checkExpects(res, xs)
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: got %v; want %q", name, err, tc.want)
		}
	}
	if err := checkExpects([]byte("Error: x"), nil); err == nil {
		t.Error("a result that is not JSON: want an error")
	}
}

func TestReleaseSetPlaceholders(t *testing.T) {
	vals := map[string]string{"<LAB>": `/tmp/lab "x"`, "<SELF>": "relseed", "<PEER>": "relpeer", "<SESSION_ID>": "session:1"}
	got, err := fillPlaceholders([]byte(`{"repo":"<LAB>/repos/demo","to":["<SELF>","<PEER>"],"s":"<SESSION_ID>"}`), vals)
	if err != nil {
		t.Fatal(err)
	}
	// The set bytes can hold an escaped placeholder: Go encodes < and > as \u003c and \u003e.
	if esc, err := fillPlaceholders([]byte(`{"to":"\u003cSELF\u003e","\u003cPEER\u003e":1}`), vals); err != nil || string(esc) != `{"relpeer":1,"to":"relseed"}` {
		t.Errorf("the escaped placeholders: %s, %v", esc, err)
	}
	if _, err := fillPlaceholders([]byte(`{"to":`), vals); err == nil {
		t.Error("args that are not JSON: want an error")
	}
	// Two member names that become one name after the fill (codex-26): an error, not a lost value.
	for _, in := range []string{`{"<SELF>":1,"relseed":2}`, `{"a":{"x<PEER>":1,"xrelpeer":2}}`, `[{"<SELF>":1,"\u003cSELF\u003e":2}]`} {
		if out, err := fillPlaceholders([]byte(in), vals); err == nil {
			t.Errorf("%s: want a refusal; got %s", in, out)
		}
	}
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("%s: %v", got, err)
	}
	if v["repo"] != `/tmp/lab "x"/repos/demo` || v["s"] != "session:1" || v["to"].([]any)[1] != "relpeer" {
		t.Fatalf("%v", v)
	}
}

// Two members with one name in the input: the decoder keeps one value, so canonicalJSON and the
// placeholder fill refuse them (codex-26).
func TestReleaseNoDupNames(t *testing.T) {
	for _, in := range []string{`{"a":1,"a":2}`, `{"<SELF>":1,"<SELF>":2}`, `[{"x":{"b":[1,{"c":1,"c":2}]}}]`, `{"a":{"b":1},"a":[]}`} {
		if err := noDupNames([]byte(in)); err == nil {
			t.Errorf("%s: want a refusal", in)
		}
		if _, err := canonicalJSON([]byte(in)); err == nil {
			t.Errorf("canonicalJSON %s: want a refusal", in)
		}
	}
	for _, in := range []string{`{"a":1,"b":{"a":2},"c":[{"a":3},{"a":4}]}`, `[["a","a"],{"a":"a"}]`, `{"a":{},"b":[],"c":{"a":[]}}`, `"a"`, `1`} {
		if err := noDupNames([]byte(in)); err != nil {
			t.Errorf("%s: %v", in, err)
		}
	}
}

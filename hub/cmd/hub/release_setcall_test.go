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
	got := fillPlaceholders([]byte(`{"repo":"<LAB>/repos/demo","to":["<SELF>","<PEER>"],"s":"<SESSION_ID>"}`),
		map[string]string{"<LAB>": `/tmp/lab "x"`, "<SELF>": "relseed", "<PEER>": "relpeer", "<SESSION_ID>": "session:1"})
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("%s: %v", got, err)
	}
	if v["repo"] != `/tmp/lab "x"/repos/demo` || v["s"] != "session:1" || v["to"].([]any)[1] != "relpeer" {
		t.Fatalf("%v", v)
	}
}

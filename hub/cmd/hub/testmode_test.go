package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParityProblem(t *testing.T) {
	exports := map[string]bool{"m.send": true, "m.hidden": true}
	face := map[string]bool{"m.send": true}
	cases := []struct {
		name string
		d    actionDesc
		want string
	}{
		{"tool on the face", actionDesc{Name: "send", Tool: "m.send", toolSet: true}, ""},
		{"user_only with a reason", actionDesc{Name: "open", UserOnly: "navigation", userSet: true}, ""},
		{"exempt back", actionDesc{Name: "$back"}, ""},
		{"exempt cancel", actionDesc{Name: "$cancel"}, ""},
		{"exempt history load", actionDesc{Name: "load", Field: "load_action"}, ""},
		{"load outside history paging", actionDesc{Name: "load", Field: "submit_action"}, "names no tool"},
		{"neither field", actionDesc{Name: "open"}, "names no tool"},
		{"empty reason", actionDesc{Name: "open", UserOnly: "  ", userSet: true}, "user_only is empty"},
		{"both fields", actionDesc{Name: "x", Tool: "m.send", toolSet: true, UserOnly: "r", userSet: true}, "both"},
		{"missing export", actionDesc{Name: "x", Tool: "m.nope", toolSet: true}, "not an export"},
		{"export without agent face", actionDesc{Name: "x", Tool: "m.hidden", toolSet: true}, "not on the agent face"},
	}
	for _, c := range cases {
		got := parityProblem(c.d, exports, face)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTreeActionsFindsNestedDescriptors(t *testing.T) {
	var tree map[string]any
	json.Unmarshal([]byte(`{"title":"t","back":{"name":"$back"},"nodes":[
		{"id":"list","type":"list","items":[{"id":"a","action":{"name":"open","user_only":"navigation"}},{"id":"b","action":{"name":"open"}}]},
		{"id":"form","type":"form","submit_action":{"name":"submit","tool":"m.send"}},
		{"id":"group","nodes":[{"id":"inner","type":"widget","action":{"name":"poke","tool":7}}]},
		{"id":"composer","type":"composer","save_action":{"name":"save","user_only":""},"send_action":{"name":"send"}}]}`), &tree)
	got := map[string]actionDesc{}
	for _, d := range treeActions(tree) {
		got[d.Node+" "+d.Name] = d
	}
	for _, want := range []string{"$root $back", "list/a open", "list/b open", "form submit", "group/inner poke", "composer save", "composer send"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	exports, face := map[string]bool{"m.send": true}, map[string]bool{"m.send": true}
	if p := parityProblem(got["group/inner poke"], exports, face); !strings.Contains(p, "not an export") {
		t.Errorf("a tool that is not a string: %q", p)
	}
	if p := parityProblem(got["composer save"], exports, face); !strings.Contains(p, "empty") {
		t.Errorf("an empty reason: %q", p)
	}
	if p := parityProblem(got["form submit"], exports, face); p != "" {
		t.Errorf("a valid tool: %q", p)
	}
}

func TestParseResultLines(t *testing.T) {
	cases := []struct {
		name, out, want string
		run             string
	}{
		{"complete", "LRT plan 2\nLRT ok 1 a\nLRT ok 2 b\nLRT done 2 0\n", "", ""},
		{"no plan", "noise\n", "no plan line", ""},
		{"no done", "LRT plan 1\nLRT ok 1 a\n", "no done line", ""},
		{"short", "LRT plan 2\nLRT ok 1 a\nLRT done 1 0\n", "plan names 2 tests, 1 reported", ""},
		{"zero", "LRT plan 0\nLRT done 0 0\n", "zero tests", ""},
		{"zero with -run", "LRT plan 0\nLRT done 0 0\n", "", "x"},
		{"bad counts", "LRT plan 1\nLRT ok 1 a\nLRT done 0 1\n", "the done line says", ""},
		{"malformed", "LRT plan 1\nLRT okay\nLRT ok 1 a\nLRT done 1 0\n", "malformed", ""},
		{"after done", "LRT plan 1\nLRT ok 1 a\nLRT done 1 0\nLRT ok 2 b\n", "after the done line", ""},
		{"index out of order", "LRT plan 2\nLRT ok 2 a\nLRT ok 1 b\nLRT done 2 0\n", "out of order", ""},
		{"repeated index", "LRT plan 2\nLRT ok 1 a\nLRT fail 1 b # x\nLRT done 1 1\n", "out of order", ""},
	}
	for _, c := range cases {
		o := &testOpts{run: c.run}
		res := &fileResult{}
		o.parse(strings.NewReader(c.out), res, io.Discard)
		joined := strings.Join(res.problems, "; ")
		if c.want == "" && joined != "" || c.want != "" && !strings.Contains(joined, c.want) {
			t.Errorf("%s: problems %q, want %q", c.name, joined, c.want)
		}
	}
}

func TestLabProofRefusals(t *testing.T) {
	for _, server := range []string{
		"unix:///Users/x/Library/Application%20Support/rex/server.sock",
		"unix:///tmp/no-such-lab/rex.sock",
		"unix:///tmp/somewhere/other.sock",
		"http://127.0.0.1:1",
	} {
		if err := checkLabProof(server); err == nil {
			t.Errorf("%s: accepted", server)
		}
	}
}

// A positive proof: lab.json beside the socket names a live process whose argv lists that socket
// and the lab state directory. A stand-in process carries those arguments.
func TestLabProofAccepts(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "lrt-proof-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "rex.sock")
	cmd := exec.Command("/bin/sh", "-c", "sleep 30; true", "sh", "--listen", "unix://"+sock, "--state-dir", filepath.Join(dir, "state"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	write := func(socket string, pid int) {
		b, _ := json.Marshal(map[string]any{"socket": socket, "server_pid": pid, "nonce": "n"})
		os.WriteFile(filepath.Join(dir, "lab.json"), b, 0o600)
	}
	write("unix://"+sock, cmd.Process.Pid)
	if err := checkLabProof("unix://" + sock); err != nil {
		t.Fatalf("refused a valid proof: %v", err)
	}
	write("unix://"+dir+"/other.sock", cmd.Process.Pid)
	if checkLabProof("unix://"+sock) == nil {
		t.Error("accepted a proof for another socket")
	}
	write("unix://"+sock, os.Getpid()) // alive, but not the lab server
	if checkLabProof("unix://"+sock) == nil {
		t.Error("accepted a process that is not the lab server")
	}
}

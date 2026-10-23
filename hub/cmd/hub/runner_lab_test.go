package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A malformed parity_routes argument fails the file in the runner's setup: one plan, one setup
// failure with the decode error, and the done line. lazyrex-hub test always sends valid JSON
// (json.Marshal), so the test starts runner.lua itself. The decode comes before the hub setup,
// so the lab needs no hub.
func TestRunnerMalformedParityRoutesLab(t *testing.T) {
	l := rexLab(t)
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("rex", "-S", l.server, "--autostart=false", "--timeout", "30s", "do",
		filepath.Join(root, "lab", "test", "runner.lua"), "file="+filepath.Join(root, "tests", "harness", "tests", "assertions.lua"),
		"lib="+filepath.Join(root, "lab", "test"), "parity_routes=[\"/a\",")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("runner: %v\nstdout:\n%s\nstderr:\n%s", err, out, stderr.Bytes())
	}
	var lines []string
	for _, s := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(s, "LRT ") {
			lines = append(lines, s)
		}
	}
	if len(lines) != 3 || lines[0] != "LRT plan 1" || lines[2] != "LRT done 0 1" {
		t.Fatalf("runner lines %q, want plan 1, one setup failure, done 0 1", lines)
	}
	if want := "LRT fail 1 setup # internal: parity_routes: invalid JSON: "; !strings.HasPrefix(lines[1], want) {
		t.Fatalf("the setup failure is %q, want the prefix %q", lines[1], want)
	}
	if !strings.Contains(lines[1], `; text starts with ["/a",`) {
		t.Fatalf("the setup failure %q does not show the text", lines[1])
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the runner took %v: the setup failure must come before any hub wait", d)
	}
}

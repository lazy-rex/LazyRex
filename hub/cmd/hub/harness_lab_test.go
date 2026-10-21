package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Lab tests of `lazyrex-hub test` itself. Each run starts its own isolated lab servers under /tmp.
// Lab only: HARNESS_LAB=1 (they need the rex CLI and take a few minutes).
func harnessBinary(t *testing.T) (bin, root string) {
	t.Helper()
	if os.Getenv("HARNESS_LAB") != "1" {
		t.Skip("set HARNESS_LAB=1 to run the harness lab tests")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(t.TempDir(), "lazyrex-hub")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin, root
}

func runHarness(t *testing.T, bin, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, append(append([]string{"test"}, args...), dir)...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

var keptLab = regexp.MustCompile(`lab kept: (/tmp/lrt-[0-9]+)`)

// assertLabsGone checks that no process names a kept lab directory, then removes the directories.
func assertLabsGone(t *testing.T, out string) {
	t.Helper()
	ps, err := exec.Command("ps", "-eww", "-o", "pid=,command=").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range keptLab.FindAllStringSubmatch(out, -1) {
		sc := bufio.NewScanner(strings.NewReader(string(ps)))
		for sc.Scan() {
			if strings.Contains(sc.Text(), m[1]+"/") {
				t.Errorf("a process of lab %s survived: %s", m[1], sc.Text())
			}
		}
		os.RemoveAll(m[1])
	}
}

func TestHarnessSelfTestsPass(t *testing.T) {
	bin, root := harnessBinary(t)
	out, code := runHarness(t, bin, filepath.Join(root, "tests", "harness"), "-keep")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	assertLabsGone(t, out)
}

// Each fixture in tests/harness-fail names the reason it must fail in its first line.
func TestHarnessFailures(t *testing.T) {
	bin, root := harnessBinary(t)
	dir := filepath.Join(root, "tests", "harness-fail")
	files, _ := filepath.Glob(filepath.Join(dir, "tests", "*.lua"))
	if len(files) == 0 {
		t.Fatal("no failure fixtures")
	}
	for _, f := range files {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, _ := os.ReadFile(f)
			first, _, _ := strings.Cut(string(b), "\n")
			want := strings.TrimPrefix(first, "-- want: ")
			args := []string{"-keep", "-file", filepath.Base(f)}
			if filepath.Base(f) == "timeout.lua" {
				args = append(args, "-timeout", "8s")
			}
			out, code := runHarness(t, bin, dir, args...)
			if code != 1 {
				t.Errorf("exit %d, want 1\n%s", code, out)
			}
			if !strings.Contains(out, want) {
				t.Errorf("output does not contain %q\n%s", want, out)
			}
			assertLabsGone(t, out)
		})
	}
}

func TestHarnessRunMatchesNothing(t *testing.T) {
	bin, root := harnessBinary(t)
	out, code := runHarness(t, bin, filepath.Join(root, "tests", "harness"), "-keep", "-file", "agents.lua", "-run", "no test has this name")
	if code != 1 || !strings.Contains(out, "matched no test") {
		t.Errorf("exit %d\n%s", code, out)
	}
	assertLabsGone(t, out)
}

// An interrupt stops the run, stops every process of the lab, and fails.
func TestHarnessInterrupt(t *testing.T) {
	bin, root := harnessBinary(t)
	cmd := exec.Command(bin, "test", "-keep", "-file", "timeout.lua", "-timeout", "120s", filepath.Join(root, "tests", "harness-fail"))
	var buf strings.Builder
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Second) // past the parity sweep, inside timeout.lua
	cmd.Process.Signal(syscall.SIGINT)
	err := cmd.Wait()
	out := buf.String()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Errorf("exit %v, want 1\n%s", err, out)
	}
	if !strings.Contains(out, "interrupted") {
		t.Errorf("no interrupt report\n%s", out)
	}
	assertLabsGone(t, out)
}

// TestHarnessIsolation runs tests/isolation with an outer HOME, XDG_CONFIG_HOME and TMPDIR in a
// temporary directory (never the real home). The outer HOME holds a sentinel; an outer variable
// holds a secret. The Lua tests check that no lab process finds the sentinel through HOME or
// XDG_CONFIG_HOME and that none gets the secret. This test checks that the outer HOME is not
// changed and that no lab file contains the secret.
func TestHarnessIsolation(t *testing.T) {
	bin, root := harnessBinary(t)
	outer := t.TempDir()
	home := filepath.Join(outer, "home")
	for _, d := range []string{filepath.Join(home, ".config"), filepath.Join(outer, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(home, ".lrt-sentinel"), filepath.Join(home, ".config", ".lrt-sentinel")} {
		if err := os.WriteFile(f, []byte("sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	listing := func() string {
		var names []string
		filepath.Walk(home, func(p string, _ os.FileInfo, _ error) error { names = append(names, p); return nil })
		return strings.Join(names, "\n")
	}
	before := listing()
	const secret = "lrt-outer-secret-7f3a"
	cmd := exec.Command(bin, "test", "-keep", filepath.Join(root, "tests", "isolation"))
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"TMPDIR="+filepath.Join(outer, "tmp"), "LRT_OUTER_SECRET="+secret)
	outb, err := cmd.CombinedOutput()
	out := string(outb)
	if err != nil || !strings.Contains(out, "tests/isolation.lua: 6 passed, 0 failed") {
		t.Fatalf("%v\n%s", err, out)
	}
	if after := listing(); after != before {
		t.Errorf("the outer HOME changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	for _, m := range keptLab.FindAllStringSubmatch(out, -1) {
		filepath.Walk(m[1], func(p string, fi os.FileInfo, err error) error {
			if err != nil || !fi.Mode().IsRegular() || fi.Size() > 8<<20 {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), secret) {
				t.Errorf("lab file %s contains the outer secret", p)
			}
			return nil
		})
	}
	assertLabsGone(t, out)
}

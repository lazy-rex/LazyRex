package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The two palette files beside the Rex config (the palette plan of the hub lead, 2026-10-22):
// setup owns lazyrex_boot.lua, the hub owns lazyrex_palette.lua, and init.lua loads both. The
// boot entries must not clash with the hub's entries, and the hub's stop file declares none.

var actionName = regexp.MustCompile(`(?m)^\s*name = "([^"]+)",$`)

func actionNames(lua string) []string {
	var names []string
	for _, m := range actionName.FindAllStringSubmatch(lua, -1) {
		names = append(names, m[1])
	}
	sort.Strings(names)
	return names
}

// testPaletteFiles are the files of one config directory: the boot file with the hub controls, and
// the hub's full file with one plugin command.
func testPaletteFiles() (boot, full string) {
	boot = setupBootLua("/bin/lazyrex-hub", "/d", "", hubControls{config: "/d/hub.json"})
	full = paletteLua(map[string][]paletteCommand{"todo": {{ID: "add", Title: "Add a todo", Route: "/add"}}})
	return boot, full
}

func TestSetupBootAndHubPaletteNames(t *testing.T) {
	boot, full := testPaletteFiles()
	bootNames, hubNames := actionNames(boot), actionNames(full)
	if want := []string{"lazyrexboot_setup", "lazyrexboot_start", "lazyrexboot_status", "lazyrexboot_stop"}; strings.Join(bootNames, " ") != strings.Join(want, " ") {
		t.Errorf("boot entries %q, want %q", bootNames, want)
	}
	if len(hubNames) < 2 || !strings.Contains(full, `name = "lazyrex_todo_add"`) {
		t.Fatalf("the hub's full file has the entries %q", hubNames)
	}
	have := map[string]bool{}
	for _, n := range bootNames {
		have[n] = true
	}
	for _, n := range hubNames {
		if have[n] {
			t.Errorf("both files declare %s", n)
		}
	}
	if n := actionNames(paletteHeader); len(n) != 0 || strings.Contains(paletteHeader, "rex.action") {
		t.Errorf("the hub's stop file declares %q", n)
	}
}

// One lab server loads init.lua as setup writes it, the boot file and the hub's full file, then
// the hub's stop file: each config load succeeds, and the palette has the boot entries every time.
// Lab only: PALETTE_LAB=1.
func TestSetupBootAndHubPaletteLab(t *testing.T) {
	if os.Getenv("PALETTE_LAB") != "1" {
		t.Skip("set PALETTE_LAB=1 to run the palette lab test (it starts its own lab server)")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	lab, err := os.MkdirTemp("/tmp", "rexlab-boot-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(lab) })
	boot, full := testPaletteFiles()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(lab, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	init := filepath.Join(lab, "init.lua")
	write("init.lua", string(appendLines(nil, `pcall(require, "lazyrex_boot")`, `pcall(require, "lazyrex_palette")`)))
	write("lazyrex_boot.lua", boot)
	write("lazyrex_palette.lua", full)
	t.Setenv("LAB_CONFIG", init)
	startLabServer(t, root, lab)
	socket := "unix://" + lab + "/rex.sock"
	rexErr := func(args ...string) (string, error) {
		out, err := exec.Command("rex", append([]string{"-S", socket, "--autostart=false", "--timeout", "15s"}, args...)...).CombinedOutput()
		return string(out), err
	}
	rex := func(args ...string) string {
		t.Helper()
		out, err := rexErr(args...)
		if err != nil {
			t.Fatalf("rex %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	check := func(when string, want, gone []string) {
		t.Helper()
		got := rex("actions")
		for _, n := range want {
			if !strings.Contains(got, n) {
				t.Errorf("%s: no %s in the palette:\n%s", when, n, got)
			}
		}
		for _, n := range gone {
			if strings.Contains(got, n) {
				t.Errorf("%s: %s is still in the palette:\n%s", when, n, got)
			}
		}
	}
	bootNames, hubNames := actionNames(boot), actionNames(full)
	check("the full file", append(append([]string(nil), bootNames...), hubNames...), nil)

	// The hub's stop: the header alone. The palette is exactly the boot file.
	write("lazyrex_palette.lua", paletteHeader)
	t.Logf("reload: %s", strings.TrimSpace(rex("config", "reload")))
	check("the stop file", bootNames, hubNames)

	// pcall(require) hides a load error. Without pcall, both files load with no error, with the
	// full file and with the stop file.
	write("init.lua", `require("lazyrex_boot")`+"\n"+`require("lazyrex_palette")`+"\n")
	t.Logf("reload without pcall, the stop file: %s", strings.TrimSpace(rex("config", "reload")))
	check("without pcall, the stop file", bootNames, hubNames)
	write("lazyrex_palette.lua", full)
	t.Logf("reload without pcall, the full file: %s", strings.TrimSpace(rex("config", "reload")))
	check("without pcall, the full file", append(append([]string(nil), bootNames...), hubNames...), nil)

	// The control: without pcall, an error in a module fails the reload as a configuration error
	// (exit 1 and Rex's error text), not as some other rex failure such as a lost connection.
	write("lazyrex_palette.lua", paletteHeader+`error("a broken palette file")`+"\n")
	out, err := rexErr("config", "reload")
	t.Logf("the control: %v: %s", err, strings.TrimSpace(out))
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(out, "lazyrex_palette.lua:3: a broken palette file") ||
		!strings.Contains(out, "the configuration has errors") {
		t.Errorf("a broken module: want exit 1 and the configuration error, got %v:\n%s", err, out)
	}
}

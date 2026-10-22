package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sdkBoot is a launcher with the contract of sdk/lua/boot.lua: it gets main=<absolute main>, puts
// the plugin directory and its own directory in package.path, and runs main.
const sdkBoot = `local main = rex.args.main
local dir = main:match('^(.*)/[^/]*$') or '.'
local sdk = debug.getinfo(1, 'S').source:gsub('^@', ''):match('^(.*)/[^/]*$') or '.'
package.path = dir .. '/?.lua;' .. dir .. '/?/init.lua;' .. sdk .. '/?.lua;' .. package.path
return dofile(main)
`

// sdkFixture is startFixture with its name from the SDK module and a sibling module.
var sdkFixture = strings.Replace(fmt.Sprintf(startFixture, "sdkp"), `local name = "sdkp"`,
	`local name = require('lazyrex').name .. require('sib')`, 1)

func writeSDK(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "sdk dir") // a space in the path
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "boot.lua"), []byte(sdkBoot), 0o600)
	os.WriteFile(filepath.Join(dir, "lazyrex.lua"), []byte("return {name = 'sdk'}\n"), 0o600)
	return dir
}

func (s *startLab) writeSDKPlugin() {
	s.writeSource("sdkp", `{"name":"sdkp","main":"main.lua","sdk":"0"}`, "")
	d := filepath.Join(s.src, "sdkp")
	os.WriteFile(filepath.Join(d, "main.lua"), []byte(sdkFixture), 0o600)
	os.WriteFile(filepath.Join(d, "sib.lua"), []byte("return 'p'\n"), 0o600)
}

// A plugin that declares sdk starts through the launcher: it requires the SDK module and a
// sibling module, and its process has main=<its main>. A plugin without sdk starts as before.
func TestPluginSDKLaunchLab(t *testing.T) {
	s := newStartLab(t, "sdkp", "plain")
	s.writeSDKPlugin()
	s.writeSource("plain", `{"name":"plain","main":"main.lua"}`, "plain")
	s.args = []string{"-sdk", writeSDK(t)}
	s.start()
	sdk, _ := s.startRun("sdkp")
	plain, _ := s.startRun("plain")
	main, _ := filepath.EvalSymlinks(filepath.Join(s.src, "sdkp", "main.lua"))
	if !argvHas(sdk.ChildPID, "main="+main) && !argvHas(sdk.ChildPID, "main="+filepath.Join(s.src, "sdkp", "main.lua")) {
		t.Fatalf("the SDK plugin process has no main=%s", main)
	}
	if argvHas(plain.ChildPID, "main="+filepath.Join(s.src, "plain", "main.lua")) {
		t.Fatal("a plugin without sdk started through the launcher")
	}
}

// Without an SDK directory, a plugin that declares sdk is refused, and the others start.
func TestPluginSDKRefusedWithoutSDKLab(t *testing.T) {
	s := newStartLab(t, "sdkp", "plain")
	s.writeSDKPlugin()
	s.writeSource("plain", `{"name":"plain","main":"main.lua"}`, "plain")
	s.args = []string{"-sdk", t.TempDir()} // no boot.lua
	s.start()
	s.startRun("plain")
	s.wait("the refusal", func() bool { return s.status("sdkp").State == "refused" })
	if st := s.status("sdkp"); !strings.Contains(st.Error, "no SDK directory") {
		t.Fatalf("the refusal: %+v", st)
	}
}

func TestManifestSDK(t *testing.T) {
	for _, c := range []struct {
		sdk string
		ok  bool
	}{{"", true}, {"0", true}, {"1", false}, {"latest", false}} {
		m := pluginManifest{Name: "p", Main: "main.lua", SDK: c.sdk}
		if err := m.check(); (err == nil) != c.ok {
			t.Errorf("sdk %q: %v", c.sdk, err)
		}
	}
}

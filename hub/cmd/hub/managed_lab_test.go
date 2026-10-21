package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Lab tests of managed entries (docs/plugin-sources.md, section 13). Lab only: HARNESS_LAB=1.

// writeManagedFile writes the default managed file <state>/plugins.json of the lab hub.
func (s *startLab) writeManagedFile(body string) {
	s.t.Helper()
	d := filepath.Join(s.l.dir, "hub")
	os.MkdirAll(d, 0o700)
	if err := os.WriteFile(filepath.Join(d, "plugins.json"), []byte(body), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// Hub start with a managed entry m that depends on the config entry a: m starts after a registers,
// and hub.plugins reports it by its id. Then a malformed managed file and a hub restart: a runs,
// m does not, and hub.plugins reports the file error once.
func TestManagedHubStartLab(t *testing.T) {
	s := newStartLab(t, "a")
	s.writeSource("a", `{"name":"a","main":"main.lua"}`, "a")
	s.writeSource("m", `{"name":"m","main":"main.lua","depends":["a"]}`, "m")
	s.writeManagedFile(fmt.Sprintf(`{"plugins":[{"id":"mid","source":%q}]}`, filepath.Join(s.src, "m")))
	s.start()
	s.registered("a")
	s.registered("m")
	before(t, s.order(), "plugin_registered a", "plugin_spawn_intent m")
	var managed map[string]any
	for _, e := range s.must("hub.plugins", nil)["entries"].([]any) {
		if e := e.(map[string]any); e["managed"] == "mid" {
			managed = e
		}
	}
	if managed == nil || managed["name"] != "m" || managed["state"] != "running" || managed["entry"] != nil {
		t.Errorf("the managed entry: %v", managed)
	}
	s.stop()
	s.writeManagedFile(`{"plugins":[{"id":"mid","source":"relative"}]}`)
	s.start()
	s.registered("a")
	refused := s.must("hub.plugins", nil)["refused"].([]any)
	if len(refused) != 1 || !strings.Contains(fmt.Sprint(refused[0]), `must be an absolute directory`) || refused[0].(map[string]any)["managed_file"] != filepath.Join(s.l.dir, "hub", "plugins.json") {
		t.Errorf("refused %v", refused)
	}
	if st := s.status("m"); st.Registered {
		t.Errorf("m runs with a malformed managed file: %+v", st)
	}
}

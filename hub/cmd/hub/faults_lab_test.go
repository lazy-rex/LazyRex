package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHubWithoutTestModeHasNoFaults starts a lab hub without -test. Its client lists no
// hub.test.* action, and hub.test.fault fails as an unknown action. Lab only: LAZYREX_LAB=1.
func TestHubWithoutTestModeHasNoFaults(t *testing.T) {
	bin, root := harnessBinary(t)
	rexBin, err := exec.LookPath("rex")
	if err != nil {
		t.Skip("no rex CLI")
	}
	dir := filepath.Join(root, "tests", "harness")
	m, err := loadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	home, err := homePlugin(root)
	if err != nil {
		t.Fatal(err)
	}
	o := &testOpts{dir: dir, lazyrex: root, home: home, self: bin, rexBin: rexBin, m: m}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	l, err := o.startLab(ctx)
	if l != nil {
		defer func() {
			for _, p := range l.stop() {
				t.Error(p)
			}
			if err := removeLab(l.dir); err != nil {
				t.Error(err)
			}
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	cfgPath, err := o.hubConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	b, _ := os.ReadFile(cfgPath)
	json.Unmarshal(b, &cfg)
	delete(cfg, "test_roots") // a hub without -test refuses test_roots and test_source_runs
	delete(cfg, "test_source_runs")
	b, _ = json.Marshal(cfg)
	os.WriteFile(cfgPath, b, 0o600)
	logf, _ := os.Create(filepath.Join(l.dir, "hub.log"))
	defer logf.Close()
	if l.hub, err = startKept(bin, "hub", []string{bin, "-config", cfgPath}, l.env, l.home(), logf, logf); err != nil {
		t.Fatal(err)
	}
	l.trackKept(l.hub)
	c := &helperConn{r: l.r}
	var hub string
	for i := 0; ; i++ {
		if hub, err = c.resolveClient(ctx, "lazyrex-hub"); err == nil {
			break
		}
		if i > 100 {
			b, _ := os.ReadFile(filepath.Join(l.dir, "hub.log"))
			t.Fatalf("no hub client: %v\n%s", err, b)
		}
		time.Sleep(150 * time.Millisecond)
	}
	var face struct {
		Actions []struct {
			Name string `json:"name"`
		} `json:"actions"`
	}
	if err := l.r.Call(ctx, "client.inspect", map[string]any{"client_id": hub}, &face); err != nil {
		t.Fatal(err)
	}
	if len(face.Actions) == 0 {
		t.Fatal("the hub client lists no actions")
	}
	for _, a := range face.Actions {
		if strings.HasPrefix(a.Name, "hub.test.") {
			t.Errorf("a hub without -test lists %s", a.Name)
		}
	}
	_, err = c.perform(ctx, hub, "hub.test.fault", map[string]any{"point": "spawn-create", "mode": "drop", "plugin": m.Name})
	// Rex refuses an action that the client does not declare (observed on the Rex build 88fd41c).
	// TestDispatchRefusesTestActionsWithoutTestMode checks the second guard, at dispatch.
	if err == nil || !strings.Contains(err.Error(), `does not offer "hub.test.fault"`) {
		t.Errorf("hub.test.fault without -test: %v", err)
	}
}

func TestDispatchRefusesTestActionsWithoutTestMode(t *testing.T) {
	h := &Hub{}
	for _, a := range []string{"hub.test.fault", "hub.test.fault_state", "hub.test.status"} {
		_, err := h.dispatch(context.Background(), perform{Action: a, Args: json.RawMessage(`{}`)})
		if err == nil || !strings.Contains(err.Error(), "unknown action "+a) {
			t.Errorf("%s: %v", a, err)
		}
	}
	if _, err := h.dispatchFaulted(context.Background(), perform{Action: "hub.test.fault", Args: json.RawMessage(`{}`)}); err == nil {
		t.Error("dispatchFaulted without -test accepted hub.test.fault")
	}
}

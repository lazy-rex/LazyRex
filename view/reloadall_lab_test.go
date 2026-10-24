package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reloadbMain is a second lab plugin, reloadb: one tab (/thread/reloadb, the text LAB-RELOADB), and
// the action reloadb.try_all, which runs hub.ui.reload {all = true} with the real rex CLI from the
// plugin's own process tree (a plugin pane) and returns its output and exit status. CMDFILE (set
// by the test) holds the argv, a JSON array.
const reloadbMain = `local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')

local function hub(action, args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if err or not res then
    error({code = 'hub_failed', message = type(err) == 'table' and (err.message or '') or tostring(err)})
  end
  return res.result
end

local obj = {type = 'object'}
rex.call('client.keymap', {actions = {
  {name = 'view.render', kind = 'client', title = 'Render', args = obj},
  {name = 'reloadb.try_all', kind = 'client', title = 'Try reload all', args = obj},
}})
rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if ev.action == 'reloadb.try_all' then
      local f = assert(io.open(CMDFILE))
      local argv = assert(rex.json.decode(f:read('*a')))
      f:close()
      table.insert(argv, 1, 'sh')
      table.insert(argv, 1, '"$@" 2>&1; echo "exit=$?"')
      table.insert(argv, 1, '-c')
      table.insert(argv, 1, 'sh')
      local r = assert(rex.exec(argv))
      local out = r.stdout or ''
      return {output = out, exit = tonumber(out:match('exit=(%d+)%s*$'))}
    end
    if ev.requester.client_id ~= hub_id then error({code = 'refused', message = 'Only the hub may call ' .. ev.action}) end
    if ev.action == 'view.render' then
      return {tree = {title = 'Reload lab', nodes = {{id = 'notice', type = 'widget', text = 'LAB-RELOADB'}}}}
    end
    if ev.action:sub(1, 5) == 'view.' then return {} end
    error({code = 'unknown_action', message = ev.action})
  end)
  if ok then
    rex.call('client.reply', {call_id = ev.call_id, result = result})
  else
    local e = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}
    rex.call('client.reply', {call_id = ev.call_id, error = e})
  end
end)

hub('hub.register', {plugin = 'reloadb', instance_id = instance_id})
hub('hub.ui.tabs', {tabs = {{key = 'lab', title = 'Reload lab tab', route = '/thread/reloadb'}}})
rex.call('client.set_label', {label = 'lazyrex-plugin:reloadb'})
`

// TestReloadAllLab (lead-devsh5, lead-devsh7): hub.ui.reload {all = true} through real Rex reloads
// the tab renderers of two plugins (modelab and reloadb) in one call: views 2, one view_reload with
// no error for each, and each pane draws its plugin's frame again. The same call from a plugin pane
// (reloadb's own process tree) gets the admin guard's refusal and reloads nothing.
func TestReloadAllLab(t *testing.T) {
	needLab(t)
	rexBin, err := exec.LookPath("rex")
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "lazyrex.json"), []byte(`{"name":"reloadb","main":"main.lua"}`), 0o600)
	// The lab's socket path is known only after newModeLab: the plugin reads the command from
	// this file at each reloadb.try_all.
	cmdFile := filepath.Join(src, "rexcmd")
	os.WriteFile(filepath.Join(src, "main.lua"), []byte("local CMDFILE = "+luaString(cmdFile)+"\n"+reloadbMain), 0o600)
	l := newModeLab(t, src)
	argv, _ := json.Marshal([]string{rexBin, "-S", l.socket, "--autostart=false", "--timeout", "10s", "-C", "lazyrex-hub", "do", "hub.ui.reload", "all=true"})
	os.WriteFile(cmdFile, argv, 0o600)

	type pane struct{ plugin, route, text, session, block string }
	panes := []*pane{{plugin: "modelab", route: "/thread/lab", text: "LAB-NOTICE"}, {plugin: "reloadb", route: "/thread/reloadb", text: "LAB-RELOADB"}}
	for _, p := range panes {
		l.plugin = p.plugin
		p.session, p.block = l.tabPane(p.route, 80, 20)
	}
	l.plugin = "modelab"
	screen := func(p *pane) string {
		out, _ := l.cli("block", "capture", "-s", p.session, "-b", p.block)
		return out
	}
	for _, p := range panes {
		l.waitFor(p.plugin+"'s frame", 30*time.Second, func() bool { return strings.Count(screen(p), p.text) == 1 })
	}
	events := filepath.Join(l.lab, "state-hub", "events.jsonl")
	reloads := func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(string(must(os.ReadFile(events))), "\n") {
			var e map[string]any
			if json.Unmarshal([]byte(line), &e) == nil && e["event"] == "view_reload" {
				out = append(out, e)
			}
		}
		return out
	}

	// From the plugin pane: the guard's text, exit 1, and no reload.
	var tried struct {
		Output string
		Exit   int
	}
	if err := json.Unmarshal([]byte(l.must("-C", "lazyrex-plugin:reloadb", "do", "reloadb.try_all")), &tried); err != nil {
		t.Fatal(err)
	}
	t.Logf("hub.ui.reload all from the plugin pane (exit %d):\n%s", tried.Exit, tried.Output)
	if !strings.Contains(tried.Output, "admin actions are refused from agent and plugin panes") || tried.Exit != 1 {
		t.Errorf("the plugin pane's call: exit %d, %q, want the admin guard's refusal and exit 1", tried.Exit, tried.Output)
	}
	time.Sleep(time.Second)
	if n := len(reloads()); n != 0 {
		t.Fatalf("the refused call reloaded %d views", n)
	}

	// From the test process (no managed pane): both plugins' renderers reload.
	out := l.must("-C", "lazyrex-hub", "do", "hub.ui.reload", "all=true")
	t.Logf("hub.ui.reload all: %s", strings.TrimSpace(out))
	var r struct {
		Status string
		Views  int
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || r.Status != "accepted" || r.Views != 2 {
		t.Fatalf("hub.ui.reload all: %q (%v), want accepted with views 2", out, err)
	}
	l.waitFor("two view_reload events", 15*time.Second, func() bool { return len(reloads()) == 2 })
	seen := map[string]bool{}
	for _, e := range reloads() {
		if e["error"] != "" {
			t.Errorf("view_reload %v: an error", e)
		}
		seen[e["plugin"].(string)] = true
	}
	if !seen["modelab"] || !seen["reloadb"] {
		t.Errorf("view_reload plugins %v, want modelab and reloadb", seen)
	}
	// The new renderer draws "Connecting…" first, then the tree.
	time.Sleep(2 * time.Second)
	for _, p := range panes {
		var last string
		l.waitFor(p.plugin+"'s frame after the reload", 30*time.Second, func() bool {
			last = screen(p)
			return strings.Count(last, p.text) == 1 && !strings.Contains(last, "Connecting")
		})
		t.Logf("%s after the reload:\n%s", p.plugin, last)
	}
}

// luaString quotes s as a Lua string literal.
func luaString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

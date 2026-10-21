package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// digestFixture registers, then answers fix.digest: it calls hub.digest with texts that a Lua
// string can hold but JSON cannot (bytes that are not UTF-8), and replies with the hub's answer,
// or the error, for each case.
const digestFixture = `local hub_id = rex.args.hub_client_id
local function hub(args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.digest', args = args, timeout_ms = 10000})
  if err then return {error = type(err) == 'table' and (tostring(err.code) .. ': ' .. tostring(err.message)) or tostring(err)} end
  return {digests = res.result.digests}
end
local function echo(s)
  local r = rex.call('target.resolve', {kinds = {'client'}, queries = {'lazyrex-plugin:e'}})
  local id = r and r.results[1] and r.results[1].target and r.results[1].target.client_id
  if not id then return {error = 'no e'} end
  local res, err = rex.call('client.perform', {client_id = id, action = 'fix.echo', args = {t = s}, timeout_ms = 10000})
  if err then return {error = tostring(err.code) .. ': ' .. tostring(err.message)} end
  return res.result
end
rex.call('client.keymap', {actions = {{name = 'fix.digest', kind = 'client', title = 'Digest', args = {type = 'object'}}}})
rex.on('client_perform_requested', function(ctx, ev)
  rex.call('client.reply', {call_id = ev.call_id, result = {
    ascii = hub({texts = {'abc'}}),
    invalid = hub({texts = {'a\255b'}}),
    truncated = hub({texts = {'\195'}}),
    utf8 = hub({texts = {'\226\130\172'}}),
    echo = echo('a\255b')}})
end)
rex.call('client.perform', {client_id = hub_id, action = 'hub.register', args = {plugin = 'd', instance_id = rex.args.instance_id, exports = {}}, timeout_ms = 10000})
rex.call('client.set_label', {label = 'lazyrex-plugin:d'})
`

// hub.digest from a Lua plugin through Rex: what reaches the hub for bytes that are not UTF-8.
// The test records the observed result; docs/hub.md, "Digests", states it.
func TestDigestLab(t *testing.T) {
	s := newStartLab(t, "d", "e")
	s.writeSource("d", `{"name":"d","main":"main.lua"}`, "")
	os.WriteFile(filepath.Join(s.src, "d", "main.lua"), []byte(digestFixture), 0o600)
	s.writeSource("e", `{"name":"e","main":"main.lua"}`, "")
	os.WriteFile(filepath.Join(s.src, "e", "main.lua"), []byte(echoFixture), 0o600)
	s.start()
	s.registered("d")
	s.registered("e")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := s.c.perform(ctx, s.status("d").ClientID, "fix.digest", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]map[string]any
	json.Unmarshal(raw, &out)
	for _, k := range []string{"ascii", "invalid", "truncated", "utf8", "echo"} {
		t.Logf("%s: %v", k, out[k])
	}
	if fmt.Sprint(out["ascii"]["digests"]) != "[ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad]" {
		t.Errorf("ascii: %v", out["ascii"])
	}
	// Observed (see docs/hub.md, "Digests"): Rex replaces each byte that is not UTF-8 with U+FFFD
	// (EF BF BD) before the receiver gets it, also between two Lua plugins. So the hub digests the
	// replaced text: sha256("a\uFFFDb") and sha256("\uFFFD").
	if fmt.Sprint(out["echo"]["hex"]) != "61efbfbd62" {
		t.Errorf("echo of a\\xffb: %v", out["echo"])
	}
	if fmt.Sprint(out["invalid"]["digests"]) != "[05087813392efc16fe8ff448920c6328e53af865df39419436659d9ffda90f7b]" {
		t.Errorf("invalid: %v", out["invalid"])
	}
	if fmt.Sprint(out["truncated"]["digests"]) != "[83d544ccc223c057d2bf80d3f2a32982c32c3c0db8e2674820da5064783fb097]" {
		t.Errorf("truncated: %v", out["truncated"])
	}
	if fmt.Sprint(out["utf8"]["digests"]) != "[c4cc90ed3d26f12d4b08a75140970a7904035c31cbb4515a83f19b9003c00d1d]" {
		t.Errorf("utf8 (U+20AC): %v", out["utf8"])
	}
}

// echoFixture answers fix.echo with the bytes of args.t, in hex: what a Lua plugin receives from
// another Lua plugin through Rex, with no Go code in between.
const echoFixture = `local hub_id = rex.args.hub_client_id
local function hex(s) return (s:gsub('.', function(c) return string.format('%02x', c:byte()) end)) end
rex.call('client.keymap', {actions = {{name = 'fix.echo', kind = 'client', title = 'Echo', args = {type = 'object'}}}})
rex.on('client_perform_requested', function(ctx, ev)
  local t = ev.args and ev.args.t or ''
  rex.call('client.reply', {call_id = ev.call_id, result = {hex = hex(t)}})
end)
rex.call('client.perform', {client_id = hub_id, action = 'hub.register', args = {plugin = 'e', instance_id = rex.args.instance_id, exports = {}}, timeout_ms = 10000})
rex.call('client.set_label', {label = 'lazyrex-plugin:e'})
`

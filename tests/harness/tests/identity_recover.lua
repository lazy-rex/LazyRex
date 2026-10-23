-- The identity recovery at hub start (hub/cmd/hub/identity.go, identityRecover): an agent process
-- that ends while the hub is down sends the hub no exit event. After the restart, its pane has no
-- current identity. A pane whose process still runs keeps its current identity. Each fake agent
-- reports with the real `lazyrex-hub hook claude` from its pane. The first one ends only after the
-- old hub process is gone, so its exit falls in the downtime.

local alice = agent('alice')
local marker = tostring(os.time())
local flags = H.args.lab .. '/recover-' .. marker

-- $1 hub binary, $2 flag directory, $3 conversation, $4 the old hub pid (0: never end).
local script = [[
hub="$1"; flags="$2"; conv="$3"; old="$4"
while [ ! -e "$flags/report" ]; do sleep 0.1; done
printf '{"hook_event_name":"SessionStart","session_id":"%s","source":"startup"}' "$conv" | "$hub" hook claude
while [ ! -e "$flags/arm" ]; do sleep 0.1; done
if [ "$old" = 0 ]; then while :; do sleep 1; done; fi
while kill -0 "$old" 2>/dev/null; do sleep 0.001; done
exit 0
]]

local function flag(name)
  rex.exec{'sh', '-c', 'mkdir -p "$0" && touch "$0/$1"', flags, name}
end

local function ident(id)
  for _, x in ipairs(H.hub('hub.identity.list', {}).identities or {}) do
    if x.identity == id then return x end
  end
  return nil
end

test('an agent exit during hub downtime: the restarted hub ends its identity; a live agent keeps its own', function()
  local old_pid = H.hub('hub.test.status', {}).pid
  local function profile(id, conv, pid)
    return {id = id, name = id, kind = 'agent',
      command = {'/bin/sh', '-c', script, 'sh', H.args.hub_bin, flags, conv, tostring(pid)}}
  end
  local gone, live = 'gone-' .. marker, 'live-' .. marker
  write_profiles({profiles = {profile('recover_gone', gone, old_pid), profile('recover_live', live, 0)}})
  local sg = alice:hub('hub.spawn', {profile = 'recover_gone', key = gone})
  local sl = alice:hub('hub.spawn', {profile = 'recover_live', key = live})
  local function worker(id)
    for _, w in ipairs(alice:call('hub.workers', {}).workers or {}) do
      if w.spawn_id == id then return w end
    end
    return {}
  end
  wait('both workers run', function()
    return worker(sg.spawn_id).state == 'running' and worker(sl.spawn_id).state == 'running'
  end, 20)
  flag('report')
  wait('both identities are current', function()
    return worker(sg.spawn_id).identity == 'current' and worker(sl.spawn_id).identity == 'current'
  end, 20)
  flag('arm')
  rex.sleep(0.2)
  H.restart_hub({crash = true})
  local w = wait('the exit of the first worker is visible after the restart', function()
    local w = worker(sg.spawn_id)
    return w.state == 'exited' and w
  end, 20)
  expect(w.identity, 'the pane of the gone process has no current identity').to_be('none')
  expect(w.identity_id, 'the gone process has no identity_id').to_be(nil)
  expect(w.identity_seen, 'the durable history stays').to_be(true)
  expect(ident('claude:' .. gone).state, 'the identity record of the gone process').to_be('detached')
  local l = worker(sl.spawn_id)
  expect(l.state, 'the live worker').to_be('running')
  expect(l.identity, 'the live process keeps its current identity').to_be('current')
  expect(l.identity_id, 'the live identity_id').to_be('claude:' .. live)
  expect(ident('claude:' .. live).state, 'the identity record of the live process').to_be('current')
end)

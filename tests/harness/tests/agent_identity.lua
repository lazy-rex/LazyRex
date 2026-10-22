-- Hub Status for an agent pane with no current conversation identity (docs/hub.md, "Hub controls").
-- A real spawn of an agent profile whose CLI does not run its hook: hub.workers says identity
-- "none", and `lazyrex-hub status` shows the advisory line. Then the CLI runs the real hook
-- command (`lazyrex-hub hook claude`) from its pane: identity "current", and the line goes away.

local function worker(a, id)
  for _, w in ipairs(a:hub('hub.workers', {}).workers) do if w.spawn_id == id then return w end end
end

-- hub_status runs Hub Status for the test hub's config and returns its output and exit code.
local function hub_status()
  local r = assert(rex.exec{'sh', '-c', '"$0" status -config "$1" 2>&1; echo "exit=$?"', H.args.hub_bin, H.args.lab .. '/hub.json'})
  local out = r.stdout or ''
  return out, tonumber(out:match('exit=(%d+)%s*$'))
end

test('a silent agent pane shows in Hub Status until its hook reports', function()
  local flag = H.args.lab .. '/silent-report-now'
  -- The fake agent CLI waits for the flag file, then runs the real hook with a SessionStart event.
  local script = 'while [ ! -e "$2" ]; do sleep 0.1; done; ' ..
    'printf \'%s\' \'{"hook_event_name":"SessionStart","session_id":"silent-1","source":"startup"}\' | "$1" hook claude; ' ..
    'exec sleep 600'
  write_profiles({profiles = {{id = 'silent', name = 'Silent agent', kind = 'agent',
    command = {'/bin/sh', '-c', script, 'sh', H.args.hub_bin, flag}}}})
  local alice = agent('alice')
  local s = alice:hub('hub.spawn', {profile = 'silent', key = 'silent-1', title = 'Silent'})
  expect(s.agent_id ~= nil, 'an agent profile gets an agent ID').to_be(true)
  local w = wait('the silent agent runs', function()
    local w = worker(alice, s.spawn_id)
    return w and w.state == 'running' and w
  end, 20)
  expect(w.identity).to_be('none')

  local out, code = hub_status()
  expect(code, out).to_be(0)
  expect(out).to_match('running: hub pid')
  expect(out).to_contain('agent Silent (pane ' .. w.block_id .. '): no current conversation identity yet.')

  -- The real hook runs from the pane: the identity becomes current, and the line goes away.
  rex.exec{'touch', flag}
  wait('the hook report', function() return worker(alice, s.spawn_id).identity == 'current' end, 20)
  out, code = hub_status()
  expect(code, out).to_be(0)
  expect(out).not_to_contain('no current conversation identity')

  -- An accessory has no field.
  local sh = alice:hub('hub.spawn', {profile = 'shell', key = 'shell-1'})
  local acc = wait('the shell runs', function()
    local w = worker(alice, sh.spawn_id)
    return w and w.state == 'running' and w
  end, 20)
  expect(acc.identity).to_be(nil)
  alice:hub('hub.stop', {spawn_id = sh.spawn_id})
  alice:hub('hub.stop', {spawn_id = s.spawn_id})
end)

-- Two real hook runs in one agent pane, the older started first (the hub orders hook reports by the
-- reporter process's start time, identity.go). The newer reports first: it becomes current. The
-- older reports after it: it is stale, records no identity, and its hook still exits 0. The pane
-- script holds the older hook's input on a FIFO until ps shows that hook's reporter child, then
-- runs the newer hook. The older hook must get its input within its own 3 s limit (hook.go
-- hookDeadline); the script polls ps 20 times, and if it does not see the reporter it records the
-- setup failure and sends no report.
test('a stale hook report: the older reporter loses, records nothing, and exits 0', function()
  local d = H.args.lab .. '/twohooks'
  local marker = tostring(os.time())
  local script = [[
d="$3"; m="$4"
mkdir -p "$d"
while [ ! -e "$2" ]; do sleep 0.1; done
mkfifo "$d/old.in"
"$1" hook codex < "$d/old.in" & hp=$!
exec 3> "$d/old.in"
n=0
while [ $n -lt 20 ]; do
  r=$(ps -A -o pid=,ppid=,args= | awk -v p="$hp" '$2 == p && /hook -reporter codex/ {print $1; exit}')
  [ -n "$r" ] && break
  sleep 0.05; n=$((n + 1))
done
if [ -z "$r" ]; then echo 'no reporter child of the older hook in 20 ps polls' > "$d/setup"; exec sleep 600; fi
echo "$r" > "$d/old.reporter"
sleep 0.1
printf '{"hook_event_name":"SessionStart","session_id":"new-%s","source":"startup"}' "$m" | "$1" hook codex
echo $? > "$d/new.exit"
printf '{"hook_event_name":"SessionStart","session_id":"old-%s","source":"clear"}' "$m" >&3
exec 3>&-
wait "$hp"; echo $? > "$d/old.exit"
exec sleep 600
]]
  local flag = H.args.lab .. '/twohooks-now'
  write_profiles({profiles = {{id = 'twohooks', name = 'Two hooks', kind = 'agent',
    command = {'/bin/sh', '-c', script, 'sh', H.args.hub_bin, flag, d, marker}}}})
  local alice = agent('alice')
  local s = alice:hub('hub.spawn', {profile = 'twohooks', key = 'twohooks-1', title = 'Two hooks'})
  wait('the agent runs', function()
    local w = worker(alice, s.spawn_id)
    return w and w.state == 'running' and w
  end, 20)
  rex.exec{'touch', flag}
  local function read(name)
    local f = io.open(d .. '/' .. name)
    if not f then return nil end
    local v = f:read('*a'); f:close()
    return (v:gsub('%s+$', ''))
  end
  wait('both hooks exited, or the setup failed', function()
    return read('setup') or (read('old.exit') and read('new.exit')) or nil
  end, 20)
  expect(read('setup'), 'the setup failure').to_be(nil)
  expect(read('old.reporter') ~= nil, 'the older reporter pid').to_be(true)
  expect(read('new.exit'), 'the newer hook exit').to_be('0')
  expect(read('old.exit'), 'the older hook exit').to_be('0')

  local state = alice:hub('hub.identity.list', {log = 30})
  local new_id, old_id = 'codex:new-' .. marker, 'codex:old-' .. marker
  local new_state, old_seen, stale = nil, false, false
  for _, e in ipairs(state.identities) do
    if e.identity == new_id then new_state = e.state end
    if e.identity == old_id then old_seen = true end
  end
  for _, e in ipairs(state.log or {}) do
    if e.identity == old_id and e.event == 'stale_report' then stale = true end
  end
  expect(new_state, 'the newer report').to_be('current')
  expect(old_seen, 'an identity for the older report').to_be(false)
  expect(stale, 'stale_report for the older report').to_be(true)
  expect(worker(alice, s.spawn_id).identity).to_be('current')
  alice:hub('hub.stop', {spawn_id = s.spawn_id})
end)

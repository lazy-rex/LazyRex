-- An agent CLI under a shell that stays (docs/hub.md, hub.workers; lead-irgap3, lead-irgap4). The
-- spawn's pane child is an interactive shell (bash with job control, set -m): it runs each fake
-- CLI as a foreground job, so the CLI leads the terminal's foreground process group. The CLI runs
-- the real hook (`lazyrex-hub hook claude`) from the pane, then exits, and the shell stays: Rex
-- sends no exit event. The hub's identity loop ends the conversation; a second CLI in the same
-- shell reports and becomes current again. (A CLI that is the pane child: identity_fields.lua.)

local marker = tostring(os.time())
local dir = H.args.lab .. '/shellchild-' .. marker

-- $1 the hub binary, $2 the flag directory, $3 the marker. cli NAME runs one fake CLI in the
-- foreground: it records its pid, reports on NAME.report, and exits on NAME.exit.
local script = [[
set -m
H="$1"; d="$2"; m="$3"
cli() {
  bash -c 'echo $$ > "$2/$3.pid"
while [ ! -e "$2/$3.report" ]; do sleep 0.1; done
printf "{\"hook_event_name\":\"SessionStart\",\"session_id\":\"%s-%s\",\"source\":\"startup\"}" "$3" "$4" | "$1" hook claude
while [ ! -e "$2/$3.exit" ]; do sleep 0.1; done' cli "$H" "$d" "$1" "$m"
  echo "$?" > "$d/$1.exited"
}
cli one
while [ ! -e "$d/two.go" ]; do sleep 0.1; done
cli two
sleep 600
]]

local function sh(cmd, ...) return assert(rex.exec{'sh', '-c', cmd, 'sh', ...}) end
local function flag(name) sh('touch "$1/$2"', dir, name) end
local function exists(name) return sh('test -e "$1/$2" && echo yes || echo no', dir, name).stdout:match('yes') ~= nil end
local function worker(a, id) return a:call('hub.workers', {spawn_id = id}).workers[1] end
local function ident(a, id)
  for _, i in ipairs(a:hub('hub.identity.list', {}).identities or {}) do
    if i.identity == id then return i end
  end
end

-- run starts one CLI's report, waits until it is current, makes it exit, and waits until the
-- conversation ends. It returns the seconds from the CLI's exit to the end.
local function run(alice, s, name)
  local id = 'claude:' .. name .. '-' .. marker
  wait(name .. ' starts', function() return exists(name .. '.pid') end, 20)
  flag(name .. '.report')
  local w = wait(name .. ' is current', function()
    local w = worker(alice, s.spawn_id)
    return w.identity_id == id and w
  end, 20)
  expect(w.identity, name .. ' after its report').to_be('current')
  expect(w.state, name .. ' after its report').to_be('running')

  flag(name .. '.exit')
  wait(name .. ' exits', function() return exists(name .. '.exited') end, 20)
  local t0 = os.time()
  w = wait(name .. ' ends', function()
    local w = worker(alice, s.spawn_id)
    return w.identity_id == nil and w
  end, 15)
  local took = os.time() - t0
  expect(w.state, 'the shell stays after ' .. name .. ' exits').to_be('running')
  expect(w.identity, name .. ' after its exit').to_be('none')
  expect(w.identity_seen, name .. ' after its exit').to_be(true)
  expect(ident(alice, id).state, name .. ' identity state').to_be('detached')
  print(string.format('OBSERVED %s: the identity ended %d s after the CLI exit', name, took))
  return took
end

test('an agent CLI under a shell that stays: its exit ends the conversation; a second CLI binds again', function()
  sh('mkdir -p "$1"', dir)
  write_profiles({profiles = {{id = 'shellcli', name = 'Shell CLI', kind = 'agent',
    command = {'/bin/bash', '-c', script, 'bash', H.args.hub_bin, dir, marker}}}})
  local alice = agent('alice')
  local s = alice:hub('hub.spawn', {profile = 'shellcli', key = 'sc-' .. marker, title = 'ShellCLI'})
  wait('the shell runs', function()
    local w = worker(alice, s.spawn_id)
    return w and w.state == 'running' and w
  end, 20)
  -- The loop checks every 2 s; os.time has a 1 s resolution.
  expect(run(alice, s, 'one') <= 5, 'the first end within the loop bound').to_be(true)
  flag('two.go')
  expect(run(alice, s, 'two') <= 5, 'the second end within the loop bound').to_be(true)
  alice:hub('hub.stop', {spawn_id = s.spawn_id})
end)

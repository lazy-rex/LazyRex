-- FACT PROBE (lead-irgap3): an agent CLI under a shell that stays. The spawn's pane child is a
-- shell; the shell runs the fake CLI in the foreground; the CLI runs the real hook from its pane;
-- then the CLI exits and the shell stays (as a login shell does after the user quits the CLI).
-- The probe prints what the hub records and shows, and checks the facts that the source predicts.

local marker = tostring(os.time())
local dir = H.args.lab .. '/shellchild-' .. marker

local script = [[
d="$2"
echo $$ > "$d/shell.pid"
sh -c 'echo $$ > "$2/cli.pid"
while [ ! -e "$2/report" ]; do sleep 0.1; done
printf "{\"hook_event_name\":\"SessionStart\",\"session_id\":\"sc-%s\",\"source\":\"startup\"}" "$3" | "$1" hook claude
while [ ! -e "$2/exit" ]; do sleep 0.1; done' cli "$1" "$2" "$3"
echo "$?" > "$d/cli.exited"
sleep 600
]]

local function sh(cmd, ...) return assert(rex.exec{'sh', '-c', cmd, 'sh', ...}) end
local function flag(name) sh('touch "$1/$2"', dir, name) end
local function read(name)
  local r = sh('cat "$1/$2" 2>/dev/null', dir, name)
  return (r.stdout or ''):gsub('%s+$', '')
end
local function worker(a, id) return a:call('hub.workers', {spawn_id = id}).workers[1] end
local function ident(a, id)
  for _, i in ipairs(a:hub('hub.identity.list', {}).identities or {}) do
    if i.identity == id then return i end
  end
end
local function show(at, w, i)
  print(string.format('OBSERVED %s: worker state=%s identity=%s identity_id=%s | identity state=%s block=%s',
    at, tostring(w and w.state), tostring(w and w.identity), tostring(w and w.identity_id),
    tostring(i and i.state), tostring(i and i.block_id)))
end

test('probe: a CLI under a shell that stays', function()
  sh('mkdir -p "$1"', dir)
  write_profiles({profiles = {{id = 'shellcli', name = 'Shell CLI', kind = 'agent',
    command = {'/bin/sh', '-c', script, 'sh', H.args.hub_bin, dir, marker}}}})
  local alice = agent('alice')
  local s = alice:hub('hub.spawn', {profile = 'shellcli', key = 'sc-' .. marker, title = 'ShellCLI'})
  wait('the shell runs', function()
    local w = worker(alice, s.spawn_id)
    return w and w.state == 'running' and read('cli.pid') ~= '' and w
  end, 20)
  local shell, cli = read('shell.pid'), read('cli.pid')
  print('OBSERVED tree: ' .. sh('ps -o pid=,ppid=,comm= -p "$1" -p "$2"', shell, cli).stdout)

  flag('report')
  local id = 'claude:sc-' .. marker
  local w = wait('the report', function()
    local w = worker(alice, s.spawn_id)
    return w.identity_id == id and w
  end, 20)
  show('after the report', w, ident(alice, id))

  flag('exit')
  wait('the CLI exits', function() return read('cli.exited') ~= '' end, 20)
  print('OBSERVED cli exit status ' .. read('cli.exited') .. '; cli alive: ' ..
    sh('kill -0 "$1" 2>/dev/null && echo yes || echo no', cli).stdout ..
    'shell alive: ' .. sh('kill -0 "$1" 2>/dev/null && echo yes || echo no', shell).stdout)
  rex.sleep(5)
  w = worker(alice, s.spawn_id)
  local i = ident(alice, id)
  show('5 s after the CLI exit', w, i)

  H.restart_hub()
  local w2 = worker(alice, s.spawn_id)
  local i2 = ident(alice, id)
  show('after a hub restart', w2, i2)

  -- The facts the source predicts: the hub records the pane child (the shell), which stays, so
  -- nothing marks the conversation gone.
  expect(w.state, 'worker state after the CLI exit').to_be('running')
  expect(w.identity_id, 'identity_id after the CLI exit').to_be(id)
  expect(i.state, 'identity state after the CLI exit').to_be('current')
  expect(w2.identity_id, 'identity_id after a hub restart').to_be(id)
  alice:hub('hub.stop', {spawn_id = s.spawn_id})
end)

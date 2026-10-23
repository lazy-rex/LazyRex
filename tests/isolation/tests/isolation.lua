-- Isolation (docs/testing.md, "Isolation"): the runner, the plugin under test, a dependency
-- plugin and a spawned worker all get the lab environment. The Go test TestHarnessIsolation runs
-- this file with an outer HOME that holds a sentinel and an outer secret variable; a plain run
-- checks the same rules without them.

local lab = H.args.lab
local home = lab .. '/home'
local alice = agent('alice')

local function read(path)
  local f = io.open(path, 'r')
  if not f then return nil end
  local s = f:read('*a')
  f:close()
  return s
end

-- under_lab accepts the lab path and its resolved form (/tmp is /private/tmp on macOS).
local function under_lab(what, path)
  local ok = false
  for _, root in ipairs({lab, '/private' .. lab}) do
    ok = ok or (type(path) == 'string' and path:sub(1, #root + 1) == root .. '/')
  end
  expect(ok, what .. ' is under the lab: ' .. tostring(path)).to_be(true)
end

local function check_env(who, e)
  expect(e.HOME, who .. ' HOME').to_be(home)
  for _, k in ipairs({'XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_STATE_HOME', 'XDG_CACHE_HOME', 'TMPDIR'}) do
    under_lab(who .. ' ' .. k, e[k])
  end
  expect(e.REX_SERVER, who .. ' REX_SERVER').to_be('unix://' .. lab .. '/rex.sock')
  expect(e.secret, who .. ' does not get an outer variable').to_be(nil)
  expect(e.plain, who .. ' gets test.env').to_be('plain value')
  expect(e.data, who .. ' gets ${LAB} expanded').to_be(lab .. '/data')
end

test('the runner has the lab environment and the lab HOME as its working directory', function()
  check_env('the runner', {HOME = os.getenv('HOME'), XDG_CONFIG_HOME = os.getenv('XDG_CONFIG_HOME'),
    XDG_DATA_HOME = os.getenv('XDG_DATA_HOME'), XDG_STATE_HOME = os.getenv('XDG_STATE_HOME'),
    XDG_CACHE_HOME = os.getenv('XDG_CACHE_HOME'), TMPDIR = os.getenv('TMPDIR'), REX_SERVER = os.getenv('REX_SERVER'),
    secret = os.getenv('LRT_OUTER_SECRET'), plain = os.getenv('LRT_PLAIN'), data = os.getenv('LRT_DATA')})
  expect(read(os.getenv('HOME') .. '/.lrt-sentinel'), 'HOME does not reach the outer home').to_be(nil)
  expect(read(os.getenv('XDG_CONFIG_HOME') .. '/.lrt-sentinel'), 'XDG_CONFIG_HOME does not reach it').to_be(nil)
  local f = assert(io.open('runner-cwd', 'w'))
  f:write('cwd')
  f:close()
  expect(read(home .. '/runner-cwd'), 'a relative write lands in the lab HOME').to_be('cwd')
end)

test('the plugin under test and a dependency plugin have the lab environment and their args', function()
  for _, p in ipairs({{name = 'isofix', marker = lab .. '/marks'}, {name = 'isodep'}}) do
    local text = wait(p.name .. ' writes its probe file', function() return read(home .. '/' .. p.name .. '-env.json') end)
    local e = rex.json.decode(text)
    check_env(p.name, e)
    expect(e.home_sentinel, p.name .. ': HOME does not reach the outer home').to_be(false)
    expect(e.config_sentinel, p.name .. ': XDG_CONFIG_HOME does not reach it').to_be(false)
    if p.marker then expect(e.marker_dir, 'test.args reach the plugin').to_be(p.marker) end
    expect(e.marker_dir:match('/marks$') ~= nil, p.name .. ' gets its own args').to_be(true)
    expect(read(home .. '/' .. p.name .. '-cwd'), p.name .. ': a relative write lands in the lab HOME').to_be('cwd')
  end
end)

test('a spawned worker has the lab environment and the lab HOME as its cwd', function()
  local s = alice:call('hub.spawn', {profile = 'probe', key = 'iso-worker'})
  local env = wait('the worker writes its env', function()
    local t = read(home .. '/worker-env')
    if t and t:find('\n', 1, true) and read(home .. '/worker-pwd') then return t end
  end)
  local vars = {}
  for k, v in env:gmatch('([%w_]+)=([^\n]*)') do vars[k] = v end
  check_env('the worker', {HOME = vars.HOME, XDG_CONFIG_HOME = vars.XDG_CONFIG_HOME, XDG_DATA_HOME = vars.XDG_DATA_HOME,
    XDG_STATE_HOME = vars.XDG_STATE_HOME, XDG_CACHE_HOME = vars.XDG_CACHE_HOME, TMPDIR = vars.TMPDIR,
    REX_SERVER = vars.REX_SERVER, secret = vars.LRT_OUTER_SECRET, plain = vars.LRT_PLAIN, data = vars.LRT_DATA})
  under_lab('the worker cwd', (read(home .. '/worker-pwd'):gsub('%s+$', '')))
  -- The lab server has stale agent variables (test.env); the worker pane drops them
  -- (workers.go spawnArgv). TestSpawnArgvEnv covers a stale LAZYREX_AGENT, which test.env cannot set.
  expect(os.getenv('CODEX_THREAD_ID'), 'the lab has a stale CODEX_THREAD_ID').to_be('stale-thread')
  expect(os.getenv('CLAUDECODE'), 'the lab has a stale CLAUDECODE').to_be('1')
  expect(vars.CODEX_THREAD_ID, 'the worker has no CODEX_THREAD_ID').to_be(nil)
  expect(vars.CLAUDECODE, 'the worker has no CLAUDECODE').to_be(nil)
  expect(vars.LAZYREX_AGENT, 'an accessory has no LAZYREX_AGENT').to_be(nil)
  alice:call('hub.stop', {spawn_id = s.spawn_id, key = 'iso-worker-stop'})
end)

local function profile(extra)
  local p = {id = 'probe', name = 'Probe worker', kind = 'accessory', command = {'/bin/sh', '-c', 'exec sleep 600'}}
  for k, v in pairs(extra) do p[k] = v end
  return {profiles = {p}}
end

test('in test mode the hub refuses a profile that restores the outer environment or leaves the lab', function()
  local cases = {
    {profile({env = {HOME = '/'}}), 'reserved'},
    {profile({env = {REX_SERVER = 'unix:///nowhere.sock'}}), 'reserved'},
    {profile({env = {XDG_CONFIG_HOME = home}}), 'reserved'},
    {profile({env = {TMPDIR = home}}), 'reserved'},
    {profile({env = {PATH = '/bin'}}), 'reserved'},
    {profile({env = {DATA = '/'}}), 'outside'},
    {profile({env = {DATA = 'relative/../../x'}}), 'absolute'},
    {profile({cwd = '/'}), 'outside'},
    {profile({cwd = lab .. '-other'}), 'outside'},
  }
  for _, c in ipairs(cases) do
    write_profiles(c[1])
    expect_error(function() alice:call('hub.profiles', {}) end, c[2])
  end
  write_profiles(profile({cwd = home, env = {DATA = lab .. '/data/new'}}))
  expect(#alice:call('hub.profiles', {}).profiles >= 2, 'a profile inside the lab passes').to_be(true)
end)

test('a malformed profile file reaches the profile loader', function()
  write_profiles('{"profiles": [')
  expect_error(function() alice:call('hub.profiles', {}) end, 'profiles.json')
  write_profiles('{"profiles":[]}')
  expect(#alice:call('hub.profiles', {}).profiles, 'only the builtin shell').to_be(1)
end)

-- ${PLUGIN} in test.env is the dir of main (docs/testing.md), and the plugin under test runs from
-- that dir (hub config test_source_runs): a plugin can compare a ${PLUGIN} path with its own dir.
-- A crash respawns it (the crash backoff, an automatic start) from the same dir.
test('${PLUGIN} is the dir that the plugin under test runs from, also after a crash respawn', function()
  local function probe(what, fn)
    return wait(what, function()
      local t = read(home .. '/isofix-env.json')
      local e = t and rex.json.decode(t)
      if e and fn(e) then return e end
    end, 20)
  end
  local first = probe('isofix writes its probe file', function() return true end)
  expect(first.own, 'LRT_OWN names a file in the dir isofix runs from').to_be(first.own_dir .. '/tests/isolation.lua')
  local st = H.hub('hub.test.status', {}).plugins.isofix
  expect(st.instance_id, 'the status names the probed instance').to_be(first.instance_id)
  expect(type(st.child_pid) == 'number' and st.child_pid > 0, 'isofix has a child pid').to_be(true)
  rex.exec({'kill', '-9', tostring(math.floor(st.child_pid))})
  local again = probe('the crash backoff respawns isofix', function(e) return e.instance_id ~= first.instance_id end)
  expect(again.own_dir, 'the respawn runs from the same dir').to_be(first.own_dir)
  expect(again.own, 'LRT_OWN still names a file in its dir').to_be(again.own_dir .. '/tests/isolation.lua')
end)

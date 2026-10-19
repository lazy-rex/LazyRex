-- Ported from the Node test home.mjs (coverage map: tests/home/README.md): launching workers.
-- Both paths: the user submits a launch form (tool = 'hub.spawn'); an agent calls hub.spawn.
-- The task profile writes its two args to LAB/home/task-args.

local codex = agent('codex')
local args_file = H.args.lab .. '/home/task-args'

local function home()
  local v = user:open('home')
  v:render()
  return v
end
local function find(list, id)
  for _, x in ipairs(list or {}) do if x.spawn_id == id then return x end end
end
local function workers() return codex:call('hub.workers', {}).workers or {} end
local function read_args()
  return wait('the task worker writes its args', function()
    local f = io.open(args_file, 'r')
    if not f then return nil end
    local s = f:read('*a')
    f:close()
    return s
  end)
end

local user_spawn
test('user path: a launch starts the worker with its title and shows its tab', function()
  local op = home():action('launch/shell', 'launch', {values = {title = 'Test'}}, 'launch-shell')
  expect(op.status).to_be('succeeded')
  expect(op.result.status).to_be('open')
  user_spawn = op.result.spawn_id
  local w = find(workers(), user_spawn)
  expect(w.title).to_be('Test')
  expect(w.parent, 'the plugin launched it for the user').to_be('user')
  expect(H.node(home():render(), 'navigation_notice'), 'the hub showed the tab').to_be(nil)
end)

test('agent path: hub.spawn starts the same profile', function()
  local r = codex:call('hub.spawn', {profile = 'shell', key = 'launch-shell-agent', title = 'Test'})
  expect(r.status).to_be('open')
  local w = find(workers(), r.spawn_id)
  expect(w.title).to_be('Test')
  expect(w.parent).to_be('codex')
end)

local named
test('named inputs reach the worker as profile args, on both paths', function()
  os.remove(args_file)
  local op = home():action('launch/task', 'launch',
    {values = {title = 'Task tab', ['arg/task'] = 'Read code\nExplain findings', ['arg/mode'] = 'review'}}, 'named-args')
  expect(op.status).to_be('succeeded')
  named = op.result
  expect(read_args()).to_be('Read code\nExplain findings|review')
  os.remove(args_file)
  codex:call('hub.spawn', {profile = 'task', key = 'named-args-agent', title = 'Task tab',
    args = {task = 'Read code\nExplain findings', mode = 'review'}})
  expect(read_args()).to_be('Read code\nExplain findings|review')
end)

test('user path: the hub retry after a profile removal rebuilds the same named inputs', function()
  -- The first attempt commits the spawn and loses its reply. A gate holds the hub's retry while
  -- the test removes the profile. After the release the plugin must send the same request
  -- (args from the form values, not from the profile), or the hub refuses it.
  local lost = fault({point = 'action', action = 'hub.spawn', when = 'after', mode = 'lose-reply'})
  local gate = fault({point = 'action', action = 'hub.spawn', when = 'before', mode = 'gate', nth = 2})
  local before = #workers()
  os.remove(args_file)
  local v = home()
  local _, key = v:admit('action', {node_id = 'launch/task', action = 'launch', revision = v.revision,
    input = {values = {title = 'Rebuilt', ['arg/task'] = 'Rebuild\nthe inputs', ['arg/mode'] = 'retry'}}})
  wait('the retry reaches the gate', function() return fault_fired(gate) end)
  expect(fault_fired(lost)).to_be(true)
  expect(read_args(), 'the first attempt started the worker').to_be('Rebuild\nthe inputs|retry')
  write_profiles({profiles = {{id = 'other', name = 'Other', kind = 'accessory', command = {'/bin/sh', '-c', 'exec sleep 600'}}}})
  expect(H.node(home():render(), 'launch/task'), 'the profile is gone').to_be(nil)
  fault_release(gate)
  local op = v:settle(key)
  expect(op.status).to_be('succeeded')
  expect(op.result.status).to_be('open')
  expect(#workers(), 'the retry replayed the first spawn').to_be(before + 1)
end)

-- hub.spawn replays a used key before it loads profiles, so a retry survives a profile removal.
test('agent path: a retry after the profile was removed replays the first launch', function()
  write_profiles({profiles = {{id = 'other', name = 'Other', kind = 'accessory', command = {'/bin/sh', '-c', 'exec sleep 600'}}}})
  local before = #workers()
  local first = find(workers(), named.spawn_id)
  expect(first, 'the user launch is still listed').to_be_truthy()
  local again = codex:call('hub.spawn', {profile = 'task', key = 'named-args-agent', title = 'Task tab',
    args = {task = 'Read code\nExplain findings', mode = 'review'}})
  expect(again.status).to_be('open')
  expect(#workers(), 'no new worker').to_be(before)
  expect_error(function() codex:call('hub.spawn', {profile = 'task', key = 'named-args-new', title = 'Task tab'}) end, 'no profile')
end)

test('a changed request under a used key is refused, on both paths', function()
  -- The user path: the hub refuses a view operation key that is used with other input.
  expect_error(function() home():action('launch/shell', 'launch', {values = {title = 'Changed'}}, 'launch-shell') end, 'different arguments')
  expect_error(function() codex:call('hub.spawn', {profile = 'shell', key = 'launch-shell-agent', title = 'Changed'}) end, 'different request')
end)

test('launch inputs must be text', function()
  local v = home()
  local op = v:action('launch/shell', 'launch', {values = {title = 7}}, 'launch-bad-title')
  expect(op.status).to_be('failed')
  expect(op.error.message).to_match('title must be text')
  op = v:action('launch/shell', 'launch', {values = {title = '', ['arg/task'] = 42}}, 'launch-bad-arg')
  expect(op.status).to_be('failed')
  expect(op.error.message).to_match('must be text')
end)

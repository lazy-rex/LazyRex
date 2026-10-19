-- Ported from the Node test home.mjs (coverage map: tests/home/README.md): the home tab and its
-- tree on the real hub. Profiles come from tests/home/profiles.json; the plugins are the
-- messaging stub and the harness fixture.

local codex = agent('codex')

local function home() return user:open('home'):render() end
local function find(list, id)
  for _, x in ipairs(list or {}) do if x.id == id then return x end end
end

test('the home plugin declares one tab, home at /', function()
  expect(#tabs()).to_be(1)
  local t = tab('home')
  expect(t.title).to_be('LazyRex')
  expect(t.route).to_be('/')
end)

test('the tree lists the other plugins, the agents and a launch form per profile', function()
  local tree = home()
  expect(tree.title).to_be('LazyRex')
  local plugins = H.node(tree, 'plugins').items
  expect(find(plugins, 'lazyrex'), 'the home plugin does not list itself').to_be(nil)
  expect(find(plugins, 'messaging')).to_be_truthy()
  expect(find(plugins, 'harnessfix')).to_be_truthy()
  local agents = H.node(tree, 'agents').items
  expect(find(agents, 'user'), 'the user is not an agent item').to_be(nil)
  expect(find(agents, 'codex').label).to_be('Codex')
  expect(find(agents, 'codex').action.name, 'messaging is loaded: a Message action').to_be('message')
  local task = H.node(tree, 'launch/task')
  expect(task.label).to_be('Launch Task')
  expect(task.fields[1]).to_equal({id = 'title', label = 'Tab title (optional)', kind = 'text', required = false, value = ''})
  expect(task.fields[2]).to_equal({id = 'arg/task', label = 'Task', kind = 'multiline', required = true, value = 'Inspect changes'})
  expect(task.fields[3].value).to_be('review')
  expect(task.submit_action.tool).to_be('hub.spawn')
  expect(#H.node(tree, 'launch/shell').fields, 'no args: only the title').to_be(1)
  expect(H.node(tree, 'stop'), 'no active worker: no stop form').to_be(nil)
end)

local running, failed
test('workers: a running worker opens and can be stopped; a failed launch can be dismissed', function()
  running = codex:call('hub.spawn', {profile = 'shell', key = 'render-running', title = 'Shell 1'})
  expect(running.status).to_be('open')
  fault({point = 'spawn-create', mode = 'refuse', op = 'render-failed'})
  failed = codex:call('hub.spawn', {profile = 'shell', key = 'render-failed', title = 'Failed launch'})
  expect(failed.status).to_be('failed')
  local tree = home()
  local items = H.node(tree, 'workers').items
  local r = find(items, running.spawn_id)
  expect(r.label).to_be('Shell 1')
  expect(r.action.name).to_be('open')
  expect(r.action.user_only).to_be('navigation')
  local f = find(items, failed.spawn_id)
  expect(f.label).to_be('Failed launch · Dismiss')
  expect(f.detail).to_match('^failed · ')
  expect(f.detail).to_match('injected fault')
  expect(f.action.name).to_be('dismiss')
  expect(f.action.tool).to_be('hub.stop')
  local stop = H.node(tree, 'stop')
  expect(#stop.fields[1].options, 'only the active worker').to_be(1)
  expect(stop.fields[1].options[1].value).to_be(running.spawn_id)
  expect(stop.submit_action.tool).to_be('hub.stop')
end)

test('a worker detail shows its program status record (OSC 7501)', function()
  -- The status profile prints a root record: state idle, msg Ready (base64 in the escape).
  local w = codex:call('hub.spawn', {profile = 'status', key = 'render-status', title = 'Reports status'})
  local item = wait('the status record reaches the worker list', function()
    local x = find(H.node(home(), 'workers').items, w.spawn_id)
    if x and x.detail ~= 'running' and x.detail ~= 'starting' then return x end
  end)
  expect(item.detail).to_be('idle · Ready')
end)

test('a worker whose pane closed leaves the list and the stop options', function()
  -- The real hub lists only sending, open and failed spawns; a closed pane closes the spawn.
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'render-closed', title = 'Closed by hand'})
  expect(find(H.node(home(), 'workers').items, w.spawn_id)).to_be_truthy()
  rex.call('block.close', {session_id = w.session_id, block_id = w.block_id})
  wait('the worker leaves the list', function() return find(H.node(home(), 'workers').items, w.spawn_id) == nil end)
  for _, o in ipairs(H.node(home(), 'stop').fields[1].options) do
    expect(o.value, 'a closed worker is not a stop option').not_to_be(w.spawn_id)
  end
end)

test('a profile error keeps navigation and the worker controls', function()
  write_profiles('{"profiles": [')
  local tree = home()
  expect(H.node(tree, 'profiles_error').text).to_match('Profiles could not be loaded')
  expect(H.node(tree, 'profiles_error').text).to_match('profiles.json')
  expect(H.node(tree, 'plugins'), 'profile errors keep navigation').to_be_truthy()
  expect(H.node(tree, 'stop'), 'profile errors keep worker controls').to_be_truthy()
  expect(H.node(tree, 'launch/shell'), 'no launch form without profiles').to_be(nil)
end)

test('without profile files only the builtin shell profile remains', function()
  write_profiles('{"profiles":[]}')
  local tree = home()
  expect(H.node(tree, 'launch/shell').label, 'the builtin shell').to_be('Launch Shell')
  expect(H.node(tree, 'launch/task')).to_be(nil)
  expect(H.node(tree, 'no_profiles'), 'the hub always has the builtin shell').to_be(nil)
  expect(H.node(tree, 'profiles_error')).to_be(nil)
end)

-- The home plugin has no tab: its palette entries and the Agents… panel tree on the real hub.
-- Profiles come from tests/profiles.json; the plugins are the messaging and launcher stubs and the
-- harness fixture.

local codex = agent('codex', {kind = 'codex', title = 'Codex'})

local function panel() return user:panel('agents'):render() end
local function find(list, id)
  for _, x in ipairs(list or {}) do if x.id == id then return x end end
end
local function command_ids()
  local ids = {}
  for _, c in ipairs(H.hub('hub.test.commands', {plugin = 'lazyrex'}).commands or {}) do ids[#ids + 1] = c.id end
  return ids
end

test('the home plugin declares no tabs; its palette entries are Agents… and Stop Worker…', function()
  expect(tabs()).to_equal({})
  -- The launcher stub offers a launch (hub.spawn): no Launch Profile….
  expect(command_ids()).to_equal({'agents', 'stop'})
  local cmds = H.hub('hub.test.commands', {plugin = 'lazyrex'}).commands
  expect(cmds[1]).to_equal({id = 'agents', title = 'Agents…', route = '/panels/agents', surface = 'panel', landing = true, user_only = 'navigation'})
  expect(cmds[2]).to_equal({id = 'stop', title = 'Stop Worker…', route = '/forms/stop', action = 'hub.stop'})
end)

test('no active worker: the Stop Worker… form says so', function()
  local tree = user:panel('stop'):render()
  expect(H.node(tree, 'no_workers').text).to_be('No worker or accessory runs.')
  expect(H.node(tree, 'stop')).to_be(nil)
end)

test('the panel lists the agents with Message, without the user', function()
  local tree = panel()
  expect(tree.title).to_be('Agents')
  local agents = H.node(tree, 'agents').items
  expect(find(agents, 'user'), 'the user is not an agent item').to_be(nil)
  expect(find(agents, codex.id).label).to_be('Codex')
  expect(find(agents, codex.id).action.name, 'messaging is loaded: a Message action').to_be('message')
  expect(H.node(tree, 'plugins'), 'no plugin list: each plugin has its own palette entry').to_be(nil)
end)

local running, failed
test('workers: an active worker has Manage…; a failed launch has Dismiss', function()
  running = codex:call('hub.spawn', {profile = 'shell', key = 'render-running', title = 'Shell 1'})
  expect(running.status).to_be('open')
  fault({point = 'spawn-create', mode = 'refuse', op = 'render-failed'})
  failed = codex:call('hub.spawn', {profile = 'shell', key = 'render-failed', title = 'Failed launch'})
  expect(failed.status).to_be('failed')
  local items = H.node(panel(), 'workers').items
  local r = find(items, running.spawn_id)
  expect(r.label).to_be('Shell 1')
  expect(r.action.name).to_be('manage')
  expect(r.action.label).to_be('Manage…')
  expect(r.action.user_only).to_be('navigation')
  local f = find(items, failed.spawn_id)
  expect(f.label).to_be('Failed launch · Dismiss')
  expect(f.detail).to_match('^failed · ')
  expect(f.detail).to_match('injected fault')
  expect(f.action.name).to_be('dismiss')
  expect(f.action.tool).to_be('hub.stop')
  local stop = H.node(user:panel('stop'):render(), 'stop')
  expect(#stop.fields[1].options, 'only the active worker').to_be(1)
  expect(stop.fields[1].options[1].value).to_be(running.spawn_id)
  expect(stop.submit_action.tool).to_be('hub.stop')
end)

test('a worker menu has Open, Stop and Close', function()
  local p = user:panel('agents')
  local op = p:action('workers', 'manage', {item_id = running.spawn_id}, 'render-manage')
  expect(op.status).to_be('succeeded')
  expect(op.result).to_equal({form = '/forms/worker/' .. running.spawn_id})
  local tree = user:view('/forms/worker/' .. running.spawn_id, nil, p.session):render()
  expect(tree.title).to_be('Shell 1')
  expect(tree.menu).to_be(true)
  expect(H.node(tree, 'open').action.user_only).to_be('navigation')
  expect(H.node(tree, 'stop').action.tool).to_be('hub.stop')
  expect(H.node(tree, 'cancel').action.name).to_be('$cancel')
end)

test('a worker detail shows its program status record (OSC 7501)', function()
  -- The status profile prints a root record: state idle, msg Ready (base64 in the escape).
  local w = codex:call('hub.spawn', {profile = 'status', key = 'render-status', title = 'Reports status'})
  local p = user:panel('agents')
  local item = wait('the status record reaches the worker list', function()
    local x = find(H.node(p:render(), 'workers').items, w.spawn_id)
    if x and x.detail ~= 'running' and x.detail ~= 'starting' then return x end
  end)
  expect(item.detail).to_be('idle · Ready')
end)

test('a worker whose pane closed leaves the list and the stop options', function()
  -- The real hub lists only sending, open and failed spawns; a closed pane closes the spawn.
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'render-closed', title = 'Closed by hand'})
  local p = user:panel('agents')
  expect(find(H.node(p:render(), 'workers').items, w.spawn_id)).to_be_truthy()
  rex.call('block.close', {session_id = w.session_id, block_id = w.block_id})
  wait('the worker leaves the list', function() return find(H.node(p:render(), 'workers').items, w.spawn_id) == nil end)
  for _, o in ipairs(H.node(user:panel('stop'):render(), 'stop').fields[1].options) do
    expect(o.value, 'a closed worker is not a stop option').not_to_be(w.spawn_id)
  end
end)

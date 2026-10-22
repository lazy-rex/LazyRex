-- Read-tracking audit of the home plugin (docs/hub.md, "SQL read tracking"; the default is
-- tracked). The home plugin has no SQL: every render reads hub actions, so the hub never installs
-- a table set for it, and hub data changes render its views again. Each test primes an attached
-- view once with a render, changes hub data, then reads the renderer's cached snapshot (View:get)
-- and never asks for a render, so a missing render is seen as a stale snapshot. The file stops the
-- launcher stub first: the home plugin registers Launch Profile… only while no other plugin offers
-- a launch.

H.hub('hub.admin.stop', {plugin = 'launcher'})
wait('the home plugin registers Launch Profile…', function()
  for _, c in ipairs(H.hub('hub.test.commands', {plugin = 'lazyrex'}).commands or {}) do
    if c.id == 'launch' then return true end
  end
end)

local function find(list, id)
  for _, x in ipairs(list or {}) do if x.id == id then return x end end
end

-- view opens a palette command's view, attached, and renders it once.
local function view(command)
  local v = user:panel(command)
  v:render()
  return v
end

-- shows waits until the renderer's snapshot satisfies ok, without a render request.
local function shows(v, what, ok)
  return wait(what, function()
    local tree = v:get().tree
    if tree and ok(tree) then return tree end
  end, 15)
end

test('an agent that reports its identity is listed', function()
  -- The production path: an agent CLI's hook reports its conversation (hub.identity.report).
  local a = agent('rt', {join = false})
  local v = view('agents')
  expect(find(H.node(v:get().tree, 'agents').items, 'claude:rt-1')).to_be(nil)
  a:report({kind = 'claude', conversation_id = 'rt-1'})
  shows(v, 'the reported identity in the agent list', function(tree)
    return find(H.node(tree, 'agents').items, 'claude:rt-1')
  end)
end)

test('a worker that starts and closes changes the worker list', function()
  local v = view('agents')
  local w = agent('codex'):call('hub.spawn', {profile = 'shell', key = 'rt-worker', title = 'Tracked'})
  shows(v, 'the new worker', function(tree) return find(H.node(tree, 'workers').items, w.spawn_id) end)
  rex.call('block.close', {session_id = w.session_id, block_id = w.block_id})
  shows(v, 'the closed worker leaves', function(tree) return find(H.node(tree, 'workers').items, w.spawn_id) == nil end)
end)

test('a plugin that stops and starts changes the Message action', function()
  local v = view('agents')
  expect(find(H.node(v:get().tree, 'agents').items, 'codex').action).to_be_truthy()
  H.hub('hub.admin.stop', {plugin = 'messaging'})
  shows(v, 'no Message action without messaging', function(tree)
    return find(H.node(tree, 'agents').items, 'codex').action == nil
  end)
  H.hub('hub.admin.start', {plugin = 'messaging'})
  shows(v, 'the Message action again', function(tree)
    return find(H.node(tree, 'agents').items, 'codex').action ~= nil
  end)
end)

test('a profile file change shows after the view gets focus', function()
  local v = view('launch')
  expect(H.node(v:get().tree, 'launch/task')).to_be_truthy()
  write_profiles('{"profiles":[]}')
  v:seen('focus') -- the hub checks the profile files when a view gets focus
  shows(v, 'the task launch form leaves', function(tree) return H.node(tree, 'launch/task') == nil end)
end)

-- navigation_notice is Lua state of the plugin, not hub data. The launch form sets it, and the
-- Agents… panel shows it. The open panel must show it without a render request.
test('a launch notice reaches the open Agents… panel', function()
  local p = view('agents')
  local id = fault({point = 'action', action = 'hub.navigate.worker', when = 'before', mode = 'fail'})
  local op = view('launch'):action('launch/shell', 'launch', {values = {title = 'No navigation'}}, 'rt-launch-fail')
  expect(fault_fired(id)).to_be(true)
  expect(op.status).to_be('succeeded')
  shows(p, 'the notice in the panel', function(tree)
    local n = H.node(tree, 'navigation_notice')
    return n and n.text:match('worker started')
  end)
  view('launch'):action('launch/shell', 'launch', {values = {title = 'Navigated'}}, 'rt-launch-ok')
  shows(p, 'the next launch clears the notice', function(tree) return H.node(tree, 'navigation_notice') == nil end)
end)

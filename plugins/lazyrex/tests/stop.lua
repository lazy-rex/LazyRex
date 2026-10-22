-- Stopping and dismissing workers, and the caller checks. User paths: the Stop Worker… form, the
-- worker menu's Stop and the panel's Dismiss (tool = 'hub.stop'); an agent calls hub.stop. Hub
-- rule (workers.go stop): any agent may stop any worker (decision "Agents may act on anything").

local codex = agent('codex')

local function listed(id)
  for _, w in ipairs(codex:call('hub.workers', {}).workers or {}) do if w.spawn_id == id then return w end end
end
local function gone(id) wait(id .. ' leaves the worker list', function() return listed(id) == nil end) end
local function open_view(id)
  for _, w in ipairs(views()) do if w.view_id == id then return w end end
end
local function stop_form()
  local v = user:panel('stop')
  v:render()
  return v
end

test('user path: the Stop Worker… form stops a worker that an agent started, then closes', function()
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'stop-by-user', title = 'Agent worker'})
  local v = stop_form()
  local op = v:action('stop', 'stop', {values = {worker = w.spawn_id}}, 'stop-form-1')
  expect(op.status).to_be('succeeded')
  expect(op.result.close_form).to_be(true)
  gone(w.spawn_id)
  wait('the form closes', function() return open_view(v.id) == nil end)
end)

test('user path: the worker menu Stop stops it and returns to the panel', function()
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'stop-menu', title = 'Menu worker'})
  local p = user:panel('agents')
  p:render()
  expect(p:action('workers', 'manage', {item_id = w.spawn_id}, 'stop-menu-manage').status).to_be('succeeded')
  local m = user:view('/forms/worker/' .. w.spawn_id, nil, p.session)
  m:render()
  local op = m:action('stop', 'stop', {}, 'stop-menu-run')
  expect(op.status).to_be('succeeded')
  expect(op.result.close_form).to_be(true)
  gone(w.spawn_id)
  wait('the menu closes', function() return open_view(m.id) == nil end)
  user:view('/panels/agents', nil, p.session):render()
end)

test('a duplicate Stop with the same key replays its result; a Stop of a gone worker answers again', function()
  -- The same key on the same view: the hub answers the stored operation (the panel's Dismiss
  -- stays open after it, so it can repeat; the Stop forms close).
  fault({point = 'spawn-create', mode = 'refuse', op = 'stop-dup'})
  local f = codex:call('hub.spawn', {profile = 'shell', key = 'stop-dup', title = 'Duplicate'})
  local p = user:panel('agents')
  p:render()
  local first = p:action('workers', 'dismiss', {item_id = f.spawn_id}, 'stop-dup-key')
  expect(first.status).to_be('succeeded')
  gone(f.spawn_id)
  local again = p:action('workers', 'dismiss', {item_id = f.spawn_id}, 'stop-dup-key')
  expect(again.result).to_equal(first.result)
  expect_error(function() p:action('workers', 'dismiss', {item_id = 'other'}, 'stop-dup-key') end, 'different arguments')
  -- A new Stop of a worker that is already gone: hub.stop is idempotent by spawn_id.
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'stop-dup-run', title = 'Twice'})
  expect(stop_form():action('stop', 'stop', {values = {worker = w.spawn_id}}, 'stop-twice-1').status).to_be('succeeded')
  gone(w.spawn_id)
  local late = codex:call('hub.stop', {spawn_id = w.spawn_id, key = 'stop-twice-agent'})
  expect(late.status).to_be('closed')
end)

test('agent path: an agent stops its own worker; a replay under the same key is the same', function()
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'stop-own', title = 'Own worker'})
  local first = codex:call('hub.stop', {spawn_id = w.spawn_id, key = 'stop-own-1'})
  gone(w.spawn_id)
  expect(codex:call('hub.stop', {spawn_id = w.spawn_id, key = 'stop-own-1'})).to_equal(first)
end)

test('a failed launch is dismissed on both paths', function()
  fault({point = 'spawn-create', mode = 'refuse', op = 'stop-failed-user'})
  fault({point = 'spawn-create', mode = 'refuse', op = 'stop-failed-agent'})
  local mine = codex:call('hub.spawn', {profile = 'shell', key = 'stop-failed-user', title = 'Fails'})
  expect(mine.status).to_be('failed')
  local theirs = codex:call('hub.spawn', {profile = 'shell', key = 'stop-failed-agent', title = 'Fails too'})
  expect(theirs.status).to_be('failed')
  local p = user:panel('agents')
  p:render()
  local op = p:action('workers', 'dismiss', {item_id = mine.spawn_id}, 'stop-dismiss')
  expect(op.status).to_be('succeeded')
  gone(mine.spawn_id)
  codex:call('hub.stop', {spawn_id = theirs.spawn_id, key = 'stop-dismiss-agent'})
  gone(theirs.spawn_id)
end)

test('the Stop Worker… form needs a worker', function()
  codex:call('hub.spawn', {profile = 'shell', key = 'stop-keep-form', title = 'Keeps the form'})
  local op = stop_form():action('stop', 'stop', {values = {worker = ''}}, 'stop-empty')
  expect(op.status).to_be('failed')
  expect(op.error.message).to_match('Select a worker')
end)

test('only the hub may call the home plugin', function()
  local client = H.hub('hub.test.status', {}).plugins.lazyrex.client_id
  for _, action in ipairs({'view.render', 'view.action'}) do
    expect_error(function() H.perform(client, action, {route = '/panels/agents', user = 'user'}) end, 'Only the hub')
  end
end)

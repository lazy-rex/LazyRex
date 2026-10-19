-- Ported from the Node test home.mjs (coverage map: tests/home/README.md): stopping and dismissing
-- workers, and the caller checks. Both paths: the stop form and the dismiss action
-- (tool = 'hub.stop'); an agent calls hub.stop. Hub rule (workers.go stop): an agent stops only
-- workers that it started; the user may stop any worker.

local codex = agent('codex')

local function home()
  local v = user:open('home')
  v:render()
  return v
end
local function listed(id)
  for _, w in ipairs(codex:call('hub.workers', {}).workers or {}) do if w.spawn_id == id then return w end end
end
local function gone(id) wait(id .. ' leaves the worker list', function() return listed(id) == nil end) end

test('user path: the stop form stops a worker that an agent started', function()
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'stop-by-user', title = 'Agent worker'})
  local v = home()
  local op = v:action('stop', 'stop', {values = {worker = w.spawn_id}}, 'stop-form-1')
  expect(op.status).to_be('succeeded')
  gone(w.spawn_id)
end)

test('agent path: an agent stops its own worker; a replay under the same key is the same', function()
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'stop-own', title = 'Own worker'})
  local first = codex:call('hub.stop', {spawn_id = w.spawn_id, key = 'stop-own-1'})
  gone(w.spawn_id)
  expect(codex:call('hub.stop', {spawn_id = w.spawn_id, key = 'stop-own-1'})).to_equal(first)
end)

test('an agent can stop a worker that the user started', function()
  -- Any agent may stop any worker (decisions.md, 2026-10-19).
  local op = home():action('launch/shell', 'launch', {values = {title = 'User worker'}}, 'stop-user-launch')
  local id = op.result.spawn_id
  expect(codex:call('hub.stop', {spawn_id = id, key = 'stop-not-mine'}).status).to_be('closed')
  gone(id)
  -- A repeat through the user's stop form is closed again (stop is idempotent by spawn_id).
  local after = home():action('stop', 'stop', {values = {worker = id}}, 'stop-form-2')
  expect(after.status).to_be('succeeded')
end)

test('a failed launch is dismissed on both paths', function()
  fault({point = 'spawn-create', mode = 'refuse', op = 'stop-failed-user'})
  fault({point = 'spawn-create', mode = 'refuse', op = 'stop-failed-agent'})
  local launch = home():action('launch/shell', 'launch', {values = {title = 'Fails'}}, 'stop-failed-user')
  expect(launch.result.status).to_be('failed')
  local mine = codex:call('hub.spawn', {profile = 'shell', key = 'stop-failed-agent', title = 'Fails too'})
  expect(mine.status).to_be('failed')
  local op = home():action('workers', 'dismiss', {item_id = launch.result.spawn_id}, 'stop-dismiss')
  expect(op.status).to_be('succeeded')
  gone(launch.result.spawn_id)
  codex:call('hub.stop', {spawn_id = mine.spawn_id, key = 'stop-dismiss-agent'})
  gone(mine.spawn_id)
end)

test('the stop form needs a worker', function()
  codex:call('hub.spawn', {profile = 'shell', key = 'stop-keep-form', title = 'Keeps the form'})
  local op = home():action('stop', 'stop', {values = {worker = ''}}, 'stop-empty')
  expect(op.status).to_be('failed')
  expect(op.error.message).to_match('Select a worker')
end)

test('only the hub may call the home plugin', function()
  local client = H.hub('hub.test.status', {}).plugins.lazyrex.client_id
  for _, action in ipairs({'view.render', 'view.action'}) do
    expect_error(function() H.perform(client, action, {route = '/', user = 'user'}) end, 'Only the hub')
  end
end)

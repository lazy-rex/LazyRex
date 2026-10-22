-- The home plugin keeps a hub error's code (lazyrex.lua hub_error). A definite refusal
-- ends a launch; a transient code is retried by the hub.

local codex = agent('codex')
local function workers() return codex:call('hub.workers', {}).workers or {} end

-- The launch form is registered only while no Profiles plugin is (the stub is stopped here).
H.hub('hub.admin.stop', {plugin = 'profiles'})
wait('the home plugin registers Launch Profile…', function()
  for _, c in ipairs(H.hub('hub.test.commands', {plugin = 'lazyrex'}).commands or {}) do
    if c.id == 'launch' then return true end
  end
end)

local function home()
  local v = user:panel('launch')
  v:render()
  return v
end

test('a definite hub refusal ends a launch with the hub message', function()
  local v = home()
  local op = v:action('launch/shell', 'launch', {values = {title = string.rep('x', 61)}}, 'launch-long-title')
  expect(op.status, 'not retried').to_be('failed')
  -- Rex checks the hub.spawn args schema (title maxLength 60) before the hub sees the call.
  expect(op.error.message).to_match('more than 60')
end)

test('a transient hub error is retried and the launch succeeds', function()
  local v = home()
  local id = fault({point = 'action', action = 'hub.spawn', when = 'before', mode = 'fail', fail_code = 'unavailable'})
  local op = v:action('launch/shell', 'launch', {values = {title = 'Retried'}}, 'launch-transient')
  expect(fault_fired(id)).to_be(true)
  expect(op.status).to_be('succeeded')
  expect(op.result.status).to_be('open')
end)

test('a disconnected reply after the spawn committed is retried with the same key: one worker', function()
  local v = home()
  local before = #workers()
  local id = fault({point = 'action', action = 'hub.spawn', when = 'after', mode = 'lose-reply'})
  local op = v:action('launch/shell', 'launch', {values = {title = 'Lost reply'}}, 'launch-lost-reply')
  expect(fault_fired(id)).to_be(true)
  expect(op.status).to_be('succeeded')
  expect(#workers(), 'the retry replays the spawn').to_be(before + 1)
end)

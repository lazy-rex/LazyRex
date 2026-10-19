-- Durable grants (hub/cmd/hub/grants.go) on the real path: the plugin makes a grant while it
-- handles an agent's forward; later, outside any forward (a direct call here), it sends the exact
-- granted request and the hub admits it as that agent.

local layout = {split = {direction = 'horizontal', ratio = 0.3,
  before = {leaf = {profile = 'shell'}}, after = {leaf = {profile = 'shell', title = 'Right'}}}}
local function entries(session)
  local req = {title = 'Dev', layout = layout}
  if session then req.session = session end
  return {{action = 'hub.spawn_window', key = 'g/w1', request = req}}
end
local function owner(a, window_id)
  local parents = {}
  for _, w in ipairs(a:hub('hub.workers', {}).workers) do
    if w.window_id == window_id then parents[#parents + 1] = w.parent end
  end
  return parents
end

test('a grant made during a forward admits its exact request later, as the agent', function()
  local alice, bob = agent('alice'), agent('bob')
  local g = alice:call('fix.grant', {key = 'run/1', entries = entries()})
  expect(g.state).to_be('active')
  -- A replay in a new forward returns the same grant; other entries or another agent conflict.
  expect(alice:call('fix.grant', {key = 'run/1', entries = entries()}).grant_id).to_be(g.grant_id)
  expect_error(function() alice:call('fix.grant', {key = 'run/1', entries = entries('session:x')}) end, 'conflict')
  expect_error(function() bob:call('fix.grant', {key = 'run/1', entries = entries()}) end, 'conflict')
  -- Deferred: no forward is in flight now.
  local w = alice:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'g/w1', title = 'Dev', layout = layout})
  expect(w.status).to_be('open')
  expect(owner(alice, w.window_id)).to_equal({'alice', 'alice'})
  -- A replay of the same request returns the same window.
  expect(alice:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'g/w1', title = 'Dev', layout = layout}).window_id).to_be(w.window_id)
  expect_error(function() alice:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'g/w1', title = 'Other', layout = layout}) end, 'not the granted request')
  expect_error(function() alice:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'g/w2', title = 'Dev', layout = layout}) end, 'does not cover')
  for _, s in ipairs(alice:hub('hub.workers', {}).workers) do
    if s.window_id == w.window_id then
      -- Any agent may stop any worker (decisions.md, 2026-10-19); a repeat is closed again.
      expect(bob:hub('hub.stop', {spawn_id = s.spawn_id}).status).to_be('closed')
      expect(alice:hub('hub.stop', {spawn_id = s.spawn_id}).status).to_be('closed')
    end
  end
end)

test('a grant needs a forward in flight', function()
  local alice, bob = agent('alice'), agent('bob')
  local pid = alice:call('fix.spawn', {key = 'pid-probe'}).pid
  expect_error(function() alice:direct('fix.grant', {key = 'run/2', entries = entries(), as_pid = pid}) end, 'no agent call')
  expect_error(function() bob:call('fix.grant', {key = 'run/2', entries = entries(), as_pid = pid}) end, 'no agent call')
end)

test('a revoked grant admits nothing and stays revoked', function()
  local alice = agent('alice')
  local e = {{action = 'hub.spawn_window', key = 'g/r1', request = {layout = layout}}}
  local g = alice:call('fix.grant', {key = 'run/3', entries = e})
  expect(alice:direct('fix.grant_revoke', {grant_id = g.grant_id}).state).to_be('revoked')
  expect(alice:direct('fix.grant_revoke', {grant_id = g.grant_id}).state).to_be('revoked')
  expect_error(function() alice:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'g/r1', layout = layout}) end, 'revoked')
  local again = alice:call('fix.grant', {key = 'run/3', entries = e})
  expect(again.grant_id).to_be(g.grant_id)
  expect(again.state).to_be('revoked')
  local got = alice:direct('fix.grant_get', {key = 'run/3'})
  expect(got.state).to_be('revoked')
  expect(got.agent).to_be('alice')
end)

test('a session reference matches only the session that ensure made for the plugin', function()
  local carol = agent('carol')
  local e = {{action = 'hub.spawn_window', key = 'g/s1', request = {session = {ensure_key = 'run4'}, layout = layout}}}
  local g = carol:call('fix.grant', {key = 'run/4', entries = e})
  expect_error(function() carol:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'g/s1', session = 'session:none', layout = layout}) end, 'not made yet')
  local s = carol:direct('fix.ensure', {key = 'run4', name = 'Granted'}).session_id
  expect_error(function() carol:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'g/s1', session = session('Elsewhere'), layout = layout}) end, 'not the granted request')
  local w = carol:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'g/s1', session = s, layout = layout})
  expect(w.session_id).to_be(s)
end)

test('a grant follows its agent through a migration', function()
  local dave = agent('dave')
  local e = {{action = 'hub.spawn_window', key = 'g/m1', request = {layout = layout}}}
  local g = dave:call('fix.grant', {key = 'run/5', entries = e})
  expect(dave:report({kind = 'claude', conversation_id = 'conv-g'}).identity).to_be('claude:conv-g')
  -- The new identity replays the create; the window it admits is dave's and the identity stops it.
  expect(dave:call('fix.grant', {key = 'run/5', entries = e}).grant_id).to_be(g.grant_id)
  local w = dave:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'g/m1', layout = layout})
  for _, s in ipairs(dave:hub('hub.workers', {}).workers) do
    if s.window_id == w.window_id then expect(dave:hub('hub.stop', {spawn_id = s.spawn_id}).status).to_be('closed') end
  end
end)

-- hub:restart(): the hub stops (or crashes) and starts again on the same lab server and state.

test('a stopped hub restarts with its state; agents keep working', function()
  local alice = agent('alice')
  alice:call('fix.put', {key = 'before', value = {n = 1}})
  local old = H.hub('hub.test.status', {}).pid
  local st = hub:restart()
  expect(st.pid).not_to_be(old)
  expect(store('before').value).to_equal({n = 1})
  expect(alice:call('fix.who', {}).agent).to_be('alice')
end)

test('a crashed hub restarts; a spawn key replays and another agent stops it', function()
  local alice, bob = agent('alice'), agent('bob')
  local first = alice:hub('hub.spawn', {profile = 'shell', key = 'across'})
  hub:restart({crash = true})
  expect(alice:hub('hub.spawn', {profile = 'shell', key = 'across'}).spawn_id).to_be(first.spawn_id)
  -- Any agent may stop any worker (decisions.md, 2026-10-19); a repeat is closed again.
  expect(bob:hub('hub.stop', {spawn_id = first.spawn_id}).status).to_be('closed')
  expect(alice:hub('hub.stop', {spawn_id = first.spawn_id}).status).to_be('closed')
end)

test('a grant made before a restart admits its request after it, as the agent', function()
  local carol, bob = agent('carol'), agent('bob')
  local layout = {leaf = {profile = 'shell'}}
  local g = carol:call('fix.grant', {key = 'run/r', entries = {{action = 'hub.spawn_window', key = 'r/1', request = {layout = layout}}}})
  hub:restart({crash = true})
  local w = carol:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'r/1', layout = layout})
  expect(w.status).to_be('open')
  for _, s in ipairs(carol:hub('hub.workers', {}).workers) do
    if s.window_id == w.window_id then
      expect(s.parent).to_be('carol')
      expect(bob:hub('hub.stop', {spawn_id = s.spawn_id}).status).to_be('closed')
      expect(carol:hub('hub.stop', {spawn_id = s.spawn_id}).status).to_be('closed')
    end
  end
  -- A revoke after the restart still holds.
  expect(carol:direct('fix.grant_revoke', {grant_id = g.grant_id}).state).to_be('revoked')
  hub:restart()
  expect_error(function() carol:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'r/1', layout = layout}) end, 'revoked')
end)

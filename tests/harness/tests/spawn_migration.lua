-- Spawn rows follow identity migration: after alice's pane reports a conversation identity, the
-- new identity replays alice's spawn keys and stops alice's workers.

local function worker(a, id)
  for _, w in ipairs(a:hub('hub.workers', {}).workers) do if w.spawn_id == id then return w end end
end

test('a migrated identity replays the old key and stops the old worker', function()
  local alice, bob = agent('alice'), agent('bob')
  local first = alice:hub('hub.spawn', {profile = 'shell', key = 'mig-1'})
  local id = first.spawn_id
  expect(worker(alice, id).parent).to_be('alice')
  expect(alice:report({kind = 'claude', conversation_id = 'conv-a'}).identity).to_be('claude:conv-a')
  -- The same key from the new identity is a replay, not a second worker.
  expect(alice:hub('hub.spawn', {profile = 'shell', key = 'mig-1'}).spawn_id).to_be(id)
  expect(worker(alice, id).parent).to_be('claude:conv-a')
  expect_error(function() bob:hub('hub.stop', {spawn_id = id}) end, 'did not start')
  expect(alice:hub('hub.stop', {spawn_id = id}).status).to_be('closed')
end)

test('a migrated identity replays the old spawn_window key', function()
  local carol = agent('carol')
  local layout = {leaf = {profile = 'shell'}}
  local first = carol:hub('hub.spawn_window', {key = 'migw-1', layout = layout})
  expect(carol:report({kind = 'claude', conversation_id = 'conv-c'}).identity).to_be('claude:conv-c')
  expect(carol:hub('hub.spawn_window', {key = 'migw-1', layout = layout}).window_id).to_be(first.window_id)
end)

test('a key that both IDs used is refused after migration; the identity stops both workers', function()
  local bob, dave = agent('bob'), agent('dave')
  -- bob's pane becomes conv-x and uses key k; then conv-x resumes in a pane whose agent used k too.
  expect(bob:report({kind = 'claude', conversation_id = 'conv-x'}).identity).to_be('claude:conv-x')
  local mine = bob:hub('hub.spawn', {profile = 'shell', key = 'both'})
  local legacy = dave:hub('hub.spawn', {profile = 'shell', key = 'both'})
  expect(legacy.spawn_id).not_to_be(mine.spawn_id)
  expect(dave:report({kind = 'claude', conversation_id = 'conv-x'}).identity).to_be('claude:conv-x')
  -- A replay cannot tell which of the two requests it repeats: refused. (bob's pane no longer
  -- runs conv-x, so it calls as bob again.)
  local e = expect_error(function() dave:hub('hub.spawn', {profile = 'shell', key = 'both'}) end)
  expect(e.message).to_match('used by dave and by claude:conv%-x')
  for _, w in ipairs(dave:hub('hub.workers', {}).workers) do
    if w.spawn_id == mine.spawn_id or w.spawn_id == legacy.spawn_id then expect(w.parent).to_be('claude:conv-x') end
  end
  expect(dave:hub('hub.stop', {spawn_id = legacy.spawn_id}).status).to_be('closed')
  expect(dave:hub('hub.stop', {spawn_id = mine.spawn_id}).status).to_be('closed')
end)

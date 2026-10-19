-- Spawn rows follow identity aliases at lookup (identity.go, aliasSet). A row keeps the ID that
-- made it (hub.workers reports that recorded parent). Replay and stop accept any ID of the
-- agent: its identity and the legacy IDs that the identity took over.

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
  expect(worker(alice, id).parent).to_be('alice') -- the recorded parent does not change
  -- Any agent may stop any worker (decisions.md, 2026-10-19); a repeat is closed again.
  expect(bob:hub('hub.stop', {spawn_id = id}).status).to_be('closed')
  expect(alice:hub('hub.stop', {spawn_id = id}).status).to_be('closed')
end)

test('another conversation in the same pane does not get the first identity\'s workers', function()
  local carol = agent('carol')
  expect(carol:report({kind = 'claude', conversation_id = 'first'}).identity).to_be('claude:first')
  local w = carol:hub('hub.spawn', {profile = 'shell', key = 'own'})
  -- The pane starts another conversation: carol stays the alias of claude:first.
  expect(carol:report({kind = 'claude', conversation_id = 'second', source = 'clear'}).identity).to_be('claude:second')
  expect(carol:hub('hub.spawn', {profile = 'shell', key = 'own'}).spawn_id).not_to_be(w.spawn_id)
  -- The new conversation may still stop the old identity's worker: any agent may stop any worker.
  expect(carol:hub('hub.stop', {spawn_id = w.spawn_id}).status).to_be('closed')
end)

test('a migrated identity replays the old spawn_window key', function()
  local dave = agent('dave')
  local layout = {leaf = {profile = 'shell'}}
  local first = dave:hub('hub.spawn_window', {key = 'migw-1', layout = layout})
  expect(dave:report({kind = 'claude', conversation_id = 'conv-d'}).identity).to_be('claude:conv-d')
  expect(dave:hub('hub.spawn_window', {key = 'migw-1', layout = layout}).window_id).to_be(first.window_id)
end)

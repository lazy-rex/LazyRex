-- Spawn keys and stops across agents and conversations (identity.go). Any agent may stop any
-- worker; another conversation in the same pane does not replay the first one's spawn keys.

test('any agent stops any worker; a repeat stop is closed again', function()
  local alice, bob = agent('alice'), agent('bob')
  local id = alice:hub('hub.spawn', {profile = 'shell', key = 'mig-1'}).spawn_id
  -- Any agent may stop any worker (decision "Agents may act on anything", 2026-10-19); a repeat is closed again.
  expect(bob:hub('hub.stop', {spawn_id = id}).status).to_be('closed')
  expect(alice:hub('hub.stop', {spawn_id = id}).status).to_be('closed')
end)

test('another conversation in the same pane does not replay the first identity\'s spawn keys', function()
  local carol = agent('carol')
  expect(carol:report({kind = 'claude', conversation_id = 'first'}).identity).to_be('claude:first')
  local w = carol:hub('hub.spawn', {profile = 'shell', key = 'own'})
  -- The pane starts another conversation: carol stays the alias of claude:first.
  expect(carol:report({kind = 'claude', conversation_id = 'second', source = 'clear'}).identity).to_be('claude:second')
  expect(carol:hub('hub.spawn', {profile = 'shell', key = 'own'}).spawn_id).not_to_be(w.spawn_id)
  -- The new conversation may still stop the old identity's worker: any agent may stop any worker.
  expect(carol:hub('hub.stop', {spawn_id = w.spawn_id}).status).to_be('closed')
end)

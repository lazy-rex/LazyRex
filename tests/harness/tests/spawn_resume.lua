-- Its own file: aliases are permanent for one hub, so earlier tests would change this case.

local function worker(a, id)
  for _, w in ipairs(a:hub('hub.workers', {}).workers) do if w.spawn_id == id then return w end end
end

-- Codex's reproducer of e492c0f: the alias exists before the legacy ID makes the work. The same
-- case covers aliases from a hub before this rule: nothing is written when the alias is made.
test('a resumed identity recovers work created after its alias already exists', function()
  local alice, bob = agent('alice'), agent('bob')
  alice:report({kind = 'claude', conversation_id = 'review-resume'})
  bob:report({kind = 'claude', conversation_id = 'review-resume'})
  local first = alice:hub('hub.spawn', {profile = 'shell', key = 'after-alias'})
  expect(worker(alice, first.spawn_id).parent).to_be('alice')
  alice:report({kind = 'claude', conversation_id = 'review-resume'})
  expect(alice:hub('hub.spawn', {profile = 'shell', key = 'after-alias'}).spawn_id).to_be(first.spawn_id)
  expect(alice:hub('hub.stop', {spawn_id = first.spawn_id}).status).to_be('closed')
end)

-- A key that two IDs of one agent used before they became one is refused (identity.go,
-- callerKeyRow), also when both requests are equal. Each worker stays stoppable by spawn_id.

local function both_used(kind, first, second, call)
  local bob, dave = agent(first), agent(second)
  local conv = 'conv-' .. kind
  expect(bob:report({kind = 'claude', conversation_id = conv}).identity).to_be('claude:' .. conv)
  local mine, legacy = call(bob), call(dave)
  expect(dave:report({kind = 'claude', conversation_id = conv}).identity).to_be('claude:' .. conv)
  local e = expect_error(function() call(dave) end, 'conflict')
  -- The message names both IDs (in sorted order).
  expect(e.message).to_contain('claude:' .. conv)
  expect(e.message).to_contain(second)
  return dave, mine, legacy
end

test('a hub.spawn key that both IDs used is refused; the identity stops both workers', function()
  local dave, mine, legacy = both_used('s', 'bob', 'dave', function(a) return a:hub('hub.spawn', {profile = 'shell', key = 'both'}) end)
  expect(legacy.spawn_id).not_to_be(mine.spawn_id)
  expect(dave:hub('hub.stop', {spawn_id = legacy.spawn_id}).status).to_be('closed')
  expect(dave:hub('hub.stop', {spawn_id = mine.spawn_id}).status).to_be('closed')
end)

test('a hub.spawn_window key that both IDs used is refused', function()
  local layout = {leaf = {profile = 'shell'}}
  local _, mine, legacy = both_used('w', 'alice', 'carol', function(a) return a:hub('hub.spawn_window', {key = 'bothw', layout = layout}) end)
  expect(legacy.window_id).not_to_be(mine.window_id)
end)

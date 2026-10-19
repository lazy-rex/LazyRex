-- Agents are real lab panes: the plugin identifies them through the real hub.
test('an agent call goes through the agent face and is attributed to its pane', function()
  local alice = agent('alice')
  expect(alice:call('fix.who', {}).agent).to_be('alice')
  expect(alice:call('fix.put', {key = 'k', value = {n = 1}}).by).to_be('alice')
  expect(store('k').value).to_equal({n = 1})
end)

test('a direct plugin call from a pane is attributed too', function()
  expect(agent('bob'):direct('fix.who', {}).agent).to_be('bob')
end)

test('a caller outside any agent pane is refused by the real check', function()
  local e = expect_error(function() H.perform(H.hub('hub.test.status', {}).plugins.harnessfix.client_id, 'fix.who', {}) end, 'unknown_caller')
  expect(e.message).to_match('does not recognize')
end)

test('a conversation identity reported from a pane takes over its legacy agent', function()
  local a = agent('alice')
  local r = a:report({kind = 'claude', conversation_id = 'conv-a'})
  expect(r.identity).to_be('claude:conv-a')
  expect(a:call('fix.who', {}).agent).to_be('claude:conv-a')
end)

test('agent errors keep their code', function()
  expect_error(function() agent('bob'):call('fix.nope', {}) end, 'not_supported')
end)

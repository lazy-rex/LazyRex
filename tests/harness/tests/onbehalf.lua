-- on_behalf: a plugin that handles a forwarded agent call acts for that agent, only while the
-- forward is in flight (hub/cmd/hub/agentface.go). The refusals without a lab are in
-- onbehalf_test.go.

test('a plugin starts a worker on behalf of the forwarded agent; the agent is its parent', function()
  local alice, bob = agent('alice'), agent('bob')
  local r = alice:call('fix.spawn', {key = 'ob-1'})
  local id = r.spawn.spawn_id
  expect(id).to_be_truthy()
  local found
  for _, w in ipairs(alice:hub('hub.workers', {}).workers) do if w.spawn_id == id then found = w end end
  expect(found.parent).to_be('alice')
  -- A replay of the key in a new forward returns the same spawn (keys are per caller).
  expect(alice:call('fix.spawn', {key = 'ob-1'}).spawn.spawn_id).to_be(id)
  -- Any agent may stop any worker (decisions.md, 2026-10-19); a repeat is closed again.
  expect(bob:hub('hub.stop', {spawn_id = id}).status).to_be('closed')
  expect(alice:hub('hub.stop', {spawn_id = id}).status).to_be('closed')
end)

test("an earlier forward's pid is refused during a later forward of another agent", function()
  local alice, bob = agent('alice'), agent('bob')
  local alice_pid = alice:call('fix.spawn', {key = 'ob-2'}).pid
  local e = expect_error(function() bob:call('fix.spawn', {key = 'ob-3', as_pid = alice_pid}) end)
  expect(e.message).to_match('no agent call')
end)

test('on_behalf outside a forward is refused', function()
  local alice = agent('alice')
  local alice_pid = alice:call('fix.spawn', {key = 'ob-4'}).pid
  -- A direct call reaches the plugin without a forward.
  local e = expect_error(function() alice:direct('fix.spawn', {key = 'ob-5', as_pid = alice_pid}) end)
  expect(e.message).to_match('no agent call')
end)

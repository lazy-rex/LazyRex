-- want: fault 1 (action gate hub.store.batch before) was not released
-- A gate that the test never opens fails the file.

test('closes a gate and never opens it', function()
  local id = fault({point = 'action', action = 'hub.store.batch', when = 'before', mode = 'gate'})
  agent('alice'):call('fix.hub', {action = 'hub.store.batch', args = {ops = {{key = 'g', value = 1}}}})
  expect(fault_fired(id)).to_be(true)
end)

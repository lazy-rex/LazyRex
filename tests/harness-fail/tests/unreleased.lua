-- want: fault 1 (action gate hub.sql.exec before) was not released
-- A gate that the test never opens fails the file.

test('closes a gate and never opens it', function()
  local id = fault({point = 'action', action = 'hub.sql.exec', when = 'before', mode = 'gate'})
  agent('alice'):call('fix.hub', {action = 'hub.sql.exec', args = {sql = 'INSERT INTO kv VALUES (?, ?)', params = {'g', '1'}}})
  expect(fault_fired(id)).to_be(true)
end)

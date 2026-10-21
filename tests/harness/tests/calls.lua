-- hub.test.calls: the recorded hub service calls of a plugin, with their args.

test('calls records each hub service call of the plugin with its args', function()
  local _, base = calls('none')
  expect(base.dropped).to_be(false)
  agent('alice'):call('fix.hub', {action = 'hub.store.scan', args = {prefix = 'thread/'}})
  agent('alice'):call('fix.hub', {action = 'hub.store.scan', args = {}})
  agent('alice'):call('fix.hub', {action = 'hub.store.get', args = {keys = {'message/1'}}})
  local scans, out = calls('hub.store.scan', {after = base.next})
  expect(out.dropped).to_be(false)
  expect(#scans).to_be(2)
  expect(scans[1].args.prefix).to_be('thread/')
  expect(scans[2].args.prefix, 'no prefix: a scan of all records').to_be(nil)
  expect(scans[1].seq < scans[2].seq).to_be(true)
  local gets = calls('hub.store.get', {after = base.next})
  expect(#gets).to_be(1)
  expect(gets[1].args.keys[1]).to_be('message/1')
  expect(#calls('hub.store.scan', {after = scans[2].seq}), 'after the last scan').to_be(0)
end)

test('calls records a call that fails', function()
  local _, base = calls('none')
  local r = agent('alice'):call('fix.hub', {action = 'hub.store.scan', args = {limit = 0}})
  expect(r.error ~= nil, 'the scan fails').to_be(true)
  local scans = calls('hub.store.scan', {after = base.next})
  expect(#scans).to_be(1)
  expect(scans[1].args.limit).to_be(0)
end)

-- A call that an action fault refuses is recorded once: the hub records it before the faults.
test('calls records a call that a before fault fails, a closed gate refuses, or an after fault fails', function()
  for _, c in ipairs({{when = 'before', mode = 'fail'}, {when = 'before', mode = 'gate'}, {when = 'after', mode = 'fail'}}) do
    local _, base = calls('none')
    local id = fault({point = 'action', action = 'hub.store.scan', when = c.when, mode = c.mode})
    local r = agent('alice'):call('fix.hub', {action = 'hub.store.scan', args = {prefix = 'message/'}})
    expect(r.error ~= nil, c.when .. ' ' .. c.mode .. ': the scan fails').to_be(true)
    if c.mode == 'gate' then fault_release(id) end
    local scans, out = calls('hub.store.scan', {after = base.next})
    expect(out.dropped).to_be(false)
    expect(#scans, c.when .. ' ' .. c.mode .. ': recorded once').to_be(1)
    expect(scans[1].args.prefix).to_be('message/')
  end
end)

test('calls refuses an unknown plugin and a cursor that is not a whole number', function()
  expect_error(function() calls('hub.store.scan', {plugin = 'no-such-plugin'}) end, 'unknown plugin')
  expect_error(function() calls('hub.store.scan', {after = 1.5}) end, 'whole number')
  expect_error(function() calls('hub.store.scan', {after = -1}) end, 'whole number')
end)

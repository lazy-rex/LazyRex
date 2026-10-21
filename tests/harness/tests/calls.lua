-- hub.test.calls: the recorded calls of a plugin, with their args. The fixture plugin makes its own
-- hub calls too (for example renders), so each check selects the calls that the test sent: by a
-- prefix or key under calls-test/, or by the marker limit 7.

local function mine(list)
  local out = {}
  for _, c in ipairs(list) do
    local a = c.args
    local k = a.keys and a.keys[1]
    if (a.prefix and a.prefix:find('^calls%-test/')) or a.limit == 7 or (k and k:find('^calls%-test/')) then
      out[#out + 1] = c
    end
  end
  return out
end

test('calls records each call of the plugin with its args', function()
  local _, base = calls('none')
  expect(base.dropped).to_be(false)
  agent('alice'):call('fix.hub', {action = 'hub.store.scan', args = {prefix = 'calls-test/'}})
  agent('alice'):call('fix.hub', {action = 'hub.store.scan', args = {limit = 7}})
  agent('alice'):call('fix.hub', {action = 'hub.store.get', args = {keys = {'calls-test/1'}}})
  local all, out = calls('hub.store.scan', {after = base.next})
  expect(out.dropped).to_be(false)
  local scans = mine(all)
  expect(#scans).to_be(2)
  expect(scans[1].args.prefix).to_be('calls-test/')
  expect(scans[2].args.prefix, 'no prefix: a scan of all records').to_be(nil)
  expect(scans[1].seq < scans[2].seq).to_be(true)
  local gets = mine((calls('hub.store.get', {after = base.next})))
  expect(#gets).to_be(1)
  expect(gets[1].args.keys[1]).to_be('calls-test/1')
  expect(#mine((calls('hub.store.scan', {after = scans[2].seq}))), 'after the last scan').to_be(0)
end)

test('calls records a call that fails', function()
  local _, base = calls('none')
  local r = agent('alice'):call('fix.hub', {action = 'hub.store.scan', args = {prefix = 'calls-test/', limit = 0}})
  expect(r.error ~= nil, 'the scan fails').to_be(true)
  local scans = mine((calls('hub.store.scan', {after = base.next})))
  expect(#scans).to_be(1)
  expect(scans[1].args.limit).to_be(0)
end)

-- A call that an action fault refuses is recorded once: the hub records it before the faults.
test('calls records a call that a before fault fails, a closed gate refuses, or an after fault fails', function()
  for _, c in ipairs({{when = 'before', mode = 'fail'}, {when = 'before', mode = 'gate'}, {when = 'after', mode = 'fail'}}) do
    local _, base = calls('none')
    local id = fault({point = 'action', action = 'hub.store.scan', when = c.when, mode = c.mode, key = 'calls-test'})
    local r = agent('alice'):call('fix.hub', {action = 'hub.store.scan', args = {prefix = 'calls-test/m/', key = 'calls-test'}})
    expect(r.error ~= nil, c.when .. ' ' .. c.mode .. ': the scan fails').to_be(true)
    if c.mode == 'gate' then fault_release(id) end
    local all, out = calls('hub.store.scan', {after = base.next})
    expect(out.dropped).to_be(false)
    local scans = mine(all)
    expect(#scans, c.when .. ' ' .. c.mode .. ': recorded once').to_be(1)
    expect(scans[1].args.prefix).to_be('calls-test/m/')
  end
end)

test('calls refuses an unknown plugin and a cursor that is not a whole number', function()
  expect_error(function() calls('hub.store.scan', {plugin = 'no-such-plugin'}) end, 'unknown plugin')
  expect_error(function() calls('hub.store.scan', {after = 1.5}) end, 'whole number')
  expect_error(function() calls('hub.store.scan', {after = -1}) end, 'whole number')
end)

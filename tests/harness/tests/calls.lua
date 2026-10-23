-- hub.test.calls: the recorded calls of a plugin, with their args. The fixture plugin makes its own
-- hub calls too (for example renders), so each check selects the calls that the test sent: by a
-- first parameter under calls-test/.

local function mine(list)
  local out = {}
  for _, c in ipairs(list) do
    local p = c.args.params
    if type(p) == 'table' and type(p[1]) == 'string' and p[1]:find('^calls%-test/') then
      out[#out + 1] = c
    end
  end
  return out
end

local function query(args) return agent('alice'):call('fix.hub', {action = 'hub.sql.query', args = args}) end

test('calls records each call of the plugin with its args', function()
  local _, base = calls('none')
  expect(base.dropped).to_be(false)
  query({sql = 'SELECT ?', params = {'calls-test/1'}})
  query({sql = 'SELECT ?', params = {'calls-test/2'}})
  agent('alice'):call('fix.hub', {action = 'hub.sql.exec', args = {sql = 'INSERT INTO kv VALUES (?, ?)', params = {'calls-test/3', '1'}}})
  local all, out = calls('hub.sql.query', {after = base.next})
  expect(out.dropped).to_be(false)
  local queries = mine(all)
  expect(#queries).to_be(2)
  expect(queries[1].args.params[1]).to_be('calls-test/1')
  expect(queries[2].args.params[1]).to_be('calls-test/2')
  expect(queries[1].seq < queries[2].seq).to_be(true)
  local execs = mine((calls('hub.sql.exec', {after = base.next})))
  expect(#execs).to_be(1)
  expect(execs[1].args.params[1]).to_be('calls-test/3')
  expect(#mine((calls('hub.sql.query', {after = queries[2].seq}))), 'after the last query').to_be(0)
end)

test('calls records a call that fails', function()
  local _, base = calls('none')
  local r = query({sql = 'SELEC ?', params = {'calls-test/f'}})
  expect(r.error ~= nil, 'the query fails').to_be(true)
  local queries = mine((calls('hub.sql.query', {after = base.next})))
  expect(#queries).to_be(1)
  expect(queries[1].args.sql).to_be('SELEC ?')
end)

-- A call that an action fault refuses is recorded once: the hub records it before the faults.
test('calls records a call that a before fault fails, a closed gate refuses, or an after fault fails', function()
  for _, c in ipairs({{when = 'before', mode = 'fail'}, {when = 'before', mode = 'gate'}, {when = 'after', mode = 'fail'}}) do
    local _, base = calls('none')
    local id = fault({point = 'action', action = 'hub.sql.query', when = c.when, mode = c.mode, key = 'calls-test'})
    local r = query({sql = 'SELECT ?', params = {'calls-test/m/' .. c.when .. '-' .. c.mode}, key = 'calls-test'})
    expect(r.error ~= nil, c.when .. ' ' .. c.mode .. ': the query fails').to_be(true)
    if c.mode == 'gate' then fault_release(id) end
    local all, out = calls('hub.sql.query', {after = base.next})
    expect(out.dropped).to_be(false)
    local queries = mine(all)
    expect(#queries, c.when .. ' ' .. c.mode .. ': recorded once').to_be(1)
    expect(queries[1].args.params[1]).to_be('calls-test/m/' .. c.when .. '-' .. c.mode)
  end
end)

test('calls refuses an unknown plugin and a cursor that is not a whole number', function()
  expect_error(function() calls('hub.sql.query', {plugin = 'no-such-plugin'}) end, 'unknown plugin')
  expect_error(function() calls('hub.sql.query', {after = 1.5}) end, 'whole number')
  expect_error(function() calls('hub.sql.query', {after = -1}) end, 'whole number')
end)

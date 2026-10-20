-- inject{match = {reads = key}}: after the plugin's hub.store.get of that key has built its reply,
-- the hub commits ops, then returns the reply it built. The reply has the old state; the next read
-- has the new one. fix.hub calls a hub action as the plugin and returns {result} or {error}.

local alice = agent('alice')

local function get(keys)
  local r = alice:call('fix.hub', {action = 'hub.store.get', args = {keys = keys}})
  if r.error then return nil, r.error end
  return r.result.records
end

test('a matching get returns the old value; the next get has the new one', function()
  write({{key = 'rh/a', value = 'old'}})
  local id = inject({match = {reads = 'rh/a'}, ops = {{key = 'rh/a', value = 'new'}}})
  expect(get({'rh/a'})['rh/a'].value).to_be('old')
  expect(store('rh/a').value, 'committed before the reply returned').to_be('new')
  expect(get({'rh/a'})['rh/a'].value).to_be('new')
  expect(injection(id)).to_equal({consumed = 1, remaining = 0})
end)

test('skip lets the first matching gets pass; times acts on several', function()
  write({{key = 'rh/b', value = 0}})
  local id = inject({match = {reads = 'rh/b'}, skip = 1, times = 2, ops = {{key = 'rh/c', value = 'hit'}}})
  get({'rh/b'})
  expect(store('rh/c')).to_equal({rev = '0'})
  expect(injection(id).consumed).to_be(0)
  get({'rh/b'})
  expect(store('rh/c').value).to_be('hit')
  write({{key = 'rh/c', delete = true}})
  get({'rh/b'})
  expect(store('rh/c').value).to_be('hit')
  expect(injection(id)).to_equal({consumed = 2, remaining = 0})
  get({'rh/b'}) -- used up: no fourth commit
  expect(injection(id).consumed).to_be(2)
end)

test('a multi-key get matches once; another key, a batch and a scan do not match', function()
  local id = inject({match = {reads = 'rh/d'}, times = 2, ops = {{key = 'rh/count', value = 'x'}}})
  get({'rh/other'})
  alice:call('fix.put', {key = 'rh/d', value = 1})
  alice:call('fix.hub', {action = 'hub.store.scan', args = {}})
  expect(injection(id).consumed).to_be(0)
  get({'rh/d', 'rh/e', 'rh/d2'})
  expect(injection(id).consumed, 'one get counts once').to_be(1)
  cancel_injection(id)
end)

test('a failed injected batch fails the get, commits nothing and is not counted', function()
  write({{key = 'rh/f', value = 'kept'}})
  -- The same key twice: the batch is refused at commit.
  local id = inject({match = {reads = 'rh/f'}, ops = {{key = 'rh/g', value = 1}, {key = 'rh/g', value = 2}}})
  local records, err = get({'rh/f'})
  expect(records).to_be(nil)
  expect(err.code).to_be('injection_failed')
  expect(store('rh/g')).to_equal({rev = '0'})
  expect(injection(id).consumed).to_be(0)
  cancel_injection(id)
end)

test('a reads injection is checked when it is armed', function()
  expect_error(function() inject({match = {reads = ''}, ops = {{key = 'x', value = 1}}}) end, 'reads must be')
  expect_error(function() inject({match = {reads = 'k', writes = 'k'}, ops = {{key = 'x', value = 1}}}) end, 'alone in match')
  expect_error(function() inject({match = {reads = 'k'}, replay = true}) end, 'needs ops')
  expect_error(function() inject({match = {reads = 'k'}, ops = {{key = 'x', value = 1}}, fail = 'no'}) end, 'needs ops')
end)

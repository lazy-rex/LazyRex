-- Injections fail the plugin's matching hub.sql commit. fix.put writes the kv row key with
-- the parameter key, so match {sql_commit_writes = 'kv', sql_param = key} matches one key.

-- kv returns the decoded value of the fixture's kv row key (nil: no row).
local function kv(key)
  local row = sql('SELECT value FROM kv WHERE key = ?', {key})[1]
  return row and rex.json.decode(row.value)
end

local function match(key) return {sql_commit_writes = 'kv', sql_param = key} end

test('an injection matches only its key and is consumed once', function()
  local a = agent('alice')
  with_injection({match = match('target'), fail = 'injected target'}, 1, function()
    a:call('fix.put', {key = 'unrelated', value = 1})
    expect_error(function() a:call('fix.put', {key = 'target', value = 2}) end, 'injected target')
    a:call('fix.put', {key = 'target', value = 3})
  end)
  expect(kv('unrelated')).to_be(1)
  expect(kv('target')).to_be(3)
end)

test('cancel_injection ends an injection that has no end', function()
  local a = agent('alice')
  local id = inject({match = match('held'), fail = 'held', times = 'always'})
  expect_error(function() a:call('fix.put', {key = 'held', value = 1}) end, 'held')
  expect_error(function() a:call('fix.put', {key = 'held', value = 2}) end, 'held')
  expect(cancel_injection(id).consumed).to_be(2)
  a:call('fix.put', {key = 'held', value = 3})
  expect(kv('held')).to_be(3)
  expect(injection(id).consumed).to_be(2)
end)

test('skip lets the first matching commits pass', function()
  local a = agent('alice')
  with_injection({match = match('skipped'), skip = 2, fail = 'third write'}, 1, function()
    a:call('fix.put', {key = 'skipped', value = 1})
    a:call('fix.put', {key = 'skipped', value = 2})
    expect_error(function() a:call('fix.put', {key = 'skipped', value = 3}) end, 'third write')
  end)
  expect(kv('skipped')).to_be(2)
end)

test('cancel_injection takes an exact id', function()
  local id = inject({match = match('exact-cancel'), fail = 'cancelled later', times = 'always'})
  expect_error(function() cancel_injection(id + 0.5) end, 'no injection')
  expect(injection(id).remaining).to_be('always')
  expect(cancel_injection(id).consumed).to_be(0)
end)

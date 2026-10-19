-- 64 KiB control messages: requests are measured, large records come back in chunks.
test('a request over the limit fails before it is sent', function()
  expect_error(function() agent('alice'):call('fix.echo', {text = string.rep('y', 70000)}) end, 'request_too_large')
end)

test('a large record with heavy escaping reads back exactly, in chunks', function()
  -- 6000 copies of NUL, quote and backslash: about 60 KB of JSON, over one reply's limit.
  local unit = '\0"\\'
  agent('alice'):call('fix.fill', {key = 'big', text = unit, n = 6000})
  local r = store('big')
  expect(#r.value.text).to_be(3 * 6000)
  expect(r.value.text == string.rep(unit, 6000)).to_be(true)
end)

test('a chunk of an older revision fails with changed', function()
  agent('alice'):call('fix.fill', {key = 'big2', text = 'ab', n = 30000})
  local rev = store('big2').rev
  agent('alice'):call('fix.fill', {key = 'big2', text = 'cd', n = 30000})
  expect_error(function() H.hub('hub.test.chunk', {source = 'record', plugin = 'harnessfix', key = 'big2', rev = rev, offset = 0}) end, 'changed')
end)

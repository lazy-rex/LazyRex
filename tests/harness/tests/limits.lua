-- 64 KiB control messages: requests are measured before they are sent.
test('a request over the limit fails before it is sent', function()
  expect_error(function() agent('alice'):call('fix.echo', {text = string.rep('y', 70000)}) end, 'request_too_large')
end)

test('a large value with heavy escaping round-trips through SQL exactly', function()
  -- 4000 copies of NUL, quote and backslash: about 40 KB of JSON in the kv row. The row text is
  -- a string parameter, so its escapes are escaped again: about 60 KB in the request of the
  -- fixture and in the reply of sql().
  local unit = '\0"\\'
  agent('alice'):call('fix.fill', {key = 'big', text = unit, n = 4000})
  local row = sql('SELECT value FROM kv WHERE key = ?', {'big'})[1]
  local value = rex.json.decode(row.value)
  expect(#value.text).to_be(3 * 4000)
  expect(value.text == string.rep(unit, 4000)).to_be(true)
end)

-- A SQL reply of about 864 KB, under the hub's SQL reply budget, crosses the real transport whole.
-- This is a SQL round-trip check, not coverage of the hub's reply guard: a query ends at its own
-- budget before the guard can see a reply (replyguard_lab_test.go and replyguard_test.go cover
-- the guard). The fixture calls hub.sql.query through client.perform and reports only sizes
-- (fix.measure), because its own reply must stay under 64 KiB. unit_go is the Go-encoded size of
-- one unit in the reply: the row text is JSON, so '<' is a six-byte escape (a backslash, then
-- u003c) in it, and the reply escapes that backslash once more.
for _, c in ipairs({{'ascii', 'a', 1}, {'escaped', '<', 7}, {'multibyte', '\195\169', 2}}) do
  local name, unit, unit_go = c[1], c[2], c[3]
  test(name .. ': a SQL reply under the budget arrives in full', function()
    local alice = agent('alice')
    local prefix = name .. '-a'
    for i = 1, 18 do
      alice:call('fix.fill', {key = prefix .. i, text = unit, n = math.floor(48000 / unit_go)})
    end
    local r = alice:call('fix.measure', {action = 'hub.sql.query', timeout_ms = 8000,
      args = {sql = 'SELECT value FROM kv WHERE key > ? AND key < ?', params = {prefix, prefix .. '~'}}})
    expect(r.arrived, r.message).to_be(true)
    expect(r.lua_bytes > 800000, 'size ' .. tostring(r.lua_bytes)).to_be(true)
  end)
end

-- Probe of the control message limits of a Lua plugin (rex-protocol.md, "Rex Lua runtime").
-- It is a harness test file, not a test: copy it into tests/harness/tests/ and run
--   lazyrex-hub test -file plugin-message-limits.lua -keep -v . > probe-output.txt 2>&1
-- then delete the copy. It prints PROBE lines with the measured sizes; it asserts nothing.
-- lua_bytes is rex.json.encode of the result that reached the plugin; go_bytes is the size that
-- the hub's encoder writes for the same result (computed from the content, see go_len).
local alice = agent('alice')

-- Go's encoder writes '<', '>', '&', U+2028, U+2029 and control characters as \uXXXX (6 bytes),
-- and '"' and '\' with a backslash (2 bytes). These units are the only ones the probe uses.
local units = {
  {'ascii', 'a', 1}, {'lt', '<', 6}, {'u2028', '\226\128\168', 6}, {'e-acute', '\195\169', 2},
  {'ctl-01', '\1', 6}, {'quote-backslash', '"\\', 4},
}

test('a plugin sends at most 64 KiB', function()
  alice:call('fix.fill', {key = 'p1', text = 'a', n = 50000})
  alice:call('fix.fill', {key = 'p2', text = 'a', n = 50000})
  local ok, r = pcall(alice.call, alice, 'fix.hub', {action = 'hub.store.get', args = {keys = {'p1', 'p2'}}})
  print('PROBE send: plugin reply of two 50000-byte records: ok=' .. tostring(ok) .. ' ' .. (ok and '' or tostring(type(r) == 'table' and r.message or r)))
  ok, r = pcall(alice.call, alice, 'fix.fill', {key = 'p3', text = 'a', n = 66000})
  print('PROBE send: plugin request with 66000 bytes of text: ok=' .. tostring(ok) .. ' ' .. (ok and '' or tostring(type(r) == 'table' and r.message or r)))
end)

test('a plugin receives more than 64 KiB, for each content class', function()
  for _, u in ipairs(units) do
    for _, target in ipairs({65600, 100000}) do
      -- two records of half the target each, in one unbudgeted get
      local n = math.floor(target / 2 / u[3])
      alice:call('fix.fill', {key = 'c1', text = u[2], n = n})
      alice:call('fix.fill', {key = 'c2', text = u[2], n = n})
      local r = alice:call('fix.measure', {action = 'hub.store.get', args = {keys = {'c1', 'c2'}}, timeout_ms = 4000})
      print(string.format('PROBE receive %s: text %d bytes Go-encoded (2 x %d units): arrived=%s lua_bytes=%s code=%s',
        u[1], 2 * n * u[3], n, tostring(r.arrived), tostring(r.lua_bytes), tostring(r.code)))
    end
  end
end)

test('a plugin receives up to the hub limit', function()
  -- Records of 49960 ASCII bytes. Over the hub's limit, a hub with the reply guard answers
  -- reply_too_large; a hub without it loses its connection.
  for _, count in ipairs({4, 10, 20, 21}) do
    local keys = {}
    for i = 1, count do
      keys[i] = 'r' .. i
      alice:call('fix.fill', {key = keys[i], text = 'a', n = 49960})
    end
    local r = alice:call('fix.measure', {action = 'hub.store.get', args = {keys = keys}, timeout_ms = 4000})
    print(string.format('PROBE receive %d records: arrived=%s lua_bytes=%s code=%s message=%s', count, tostring(r.arrived),
      tostring(r.lua_bytes), tostring(r.code), tostring(r.message)))
  end
end)

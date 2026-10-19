-- Probe of the control message limits of a Lua plugin (rex-protocol.md, "Rex Lua runtime").
-- It is a harness test file, not a test: copy it into tests/harness/tests/ and run
--   lazyrex-hub test -file plugin-message-limits.lua -v
-- then delete the copy. It prints PROBE lines; it asserts nothing. The last size disconnects
-- the hub from the lab Rex (over its 1 MiB limit), so the file fails there by design.
local alice = agent('alice')

test('a plugin sends at most 64 KiB', function()
  alice:call('fix.fill', {key = 'p1', text = 'a', n = 50000})
  alice:call('fix.fill', {key = 'p2', text = 'a', n = 50000})
  local ok, r = pcall(alice.call, alice, 'fix.hub', {action = 'hub.store.get', args = {keys = {'p1', 'p2'}}})
  print('PROBE plugin reply of about 100 KB: ok=' .. tostring(ok) .. ' ' .. (ok and '' or tostring(type(r) == 'table' and r.message or r)))
  ok, r = pcall(alice.call, alice, 'fix.fill', {key = 'p3', text = 'a', n = 66000})
  print('PROBE plugin request of about 66 KB: ok=' .. tostring(ok) .. ' ' .. (ok and '' or tostring(type(r) == 'table' and r.message or r)))
end)

test('a plugin receives up to the hub limit', function()
  for _, go in ipairs({65600, 100000, 500000, 1000000, 1050000}) do
    local keys, per = {}, 50000
    for i = 1, math.ceil(go / per) do
      keys[#keys + 1] = 'r' .. i
      alice:call('fix.fill', {key = 'r' .. i, text = 'a', n = per - 40})
    end
    local r = alice:call('fix.measure', {action = 'hub.store.get', args = {keys = keys}, timeout_ms = 4000})
    print(string.format('PROBE hub reply of about %d bytes: arrived=%s lua_bytes=%s code=%s', go, tostring(r.arrived), tostring(r.lua_bytes), tostring(r.code)))
  end
end)

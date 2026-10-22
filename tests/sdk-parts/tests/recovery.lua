-- The saved-reply recovery gate: a keyed write whose reply goes in parts, a lost commit reply, a
-- retry with the same key. The SDK takes its real error path and its real replay path.

local alice = agent('alice')

-- The sha256 of 150 KiB of 's'.
local SAVED = '1ea33f603a08d9943251c23c9fb7e9ac9c456570d5798a02118e06efd3582f7c'

local function whole(r, what)
  expect(r.value.kb, what .. ': kb').to_be(150)
  expect(r.value.data.len, what .. ': the length').to_be(150 * 1024)
  expect(r.value.data.sha256, what .. ': the data').to_be(SAVED)
end

test('a lost commit reply, then a retry with the same key: one write and the whole first answer', function()
  -- The commit runs and stands; its reply to the plugin becomes disconnected.
  local f = fault({point = 'action', action = 'hub.sql.commit', when = 'after', mode = 'lose-reply', plugin = 'sp'})
  local ok, first = pcall(function() return alice:call_short('sp.save', {kb = 150, key = 'k1'}) end)
  expect(fault_fired(f), 'the fault fired').to_be(true)
  print('first call: ' .. (ok and 'answered' or ('failed: ' .. tostring(type(first) == 'table' and (first.code or first.message) or first))))
  if ok then whole(first, 'first call') end
  local again = alice:call_short('sp.save', {kb = 150, key = 'k1'})
  whole(again, 'retry')
  expect(alice:call('sp.count', {}).n, 'one write').to_be(1)
  -- A third call with the same key changes nothing either.
  whole(alice:call_short('sp.save', {kb = 150, key = 'k1'}), 'third call')
  expect(alice:call('sp.count', {}).n, 'still one write').to_be(1)
end)

test('a keyed 150 KiB answer without a fault: a retry gets the same answer and writes nothing', function()
  whole(alice:call_short('sp.save', {kb = 150, key = 'k2'}), 'first call')
  local n = alice:call('sp.count', {}).n
  whole(alice:call_short('sp.save', {kb = 150, key = 'k2'}), 'retry')
  expect(alice:call('sp.count', {}).n, 'no second write').to_be(n)
end)

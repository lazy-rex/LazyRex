-- The hub's reply guard through the real transport (docs/hub.md, "Reply size"). The fixture plugin
-- calls hub.store.get through client.perform and reports only sizes (fix.measure), because its own
-- reply must stay under 64 KiB. A get of records over the hub's 1 MiB frame limit used to close
-- the hub's connection.
local alice = agent('alice')

-- fill writes n records r1..rn of about per bytes each (Go encoding) of copies of unit, and returns
-- their keys. unit_go is the Go-encoded size of one unit.
local function fill(prefix, n, unit, unit_go, per)
  local keys = {}
  for i = 1, n do
    keys[i] = prefix .. i
    alice:call('fix.fill', {key = keys[i], text = unit, n = math.floor(per / unit_go)})
  end
  return keys
end

local function hub_state()
  local s = H.hub('hub.test.status', {})
  local ins = rex.call('client.inspect', {client_id = s.client_id})
  return s.client_id, s.pid, ins and ins.client and ins.client.connection_state
end

local function measure(keys)
  return alice:call('fix.measure', {action = 'hub.store.get', args = {keys = keys}, timeout_ms = 8000})
end

for _, c in ipairs({{'ascii', 'a', 1}, {'escaped', '<', 6}, {'multibyte', '\195\169', 2}}) do
  local name, unit, unit_go = c[1], c[2], c[3]
  test(name .. ': a reply under the limit arrives in full', function()
    local keys = fill(name .. '-a', 18, unit, unit_go, 48000) -- about 864 KB
    local r = measure(keys)
    expect(r.arrived, r.message).to_be(true)
    expect(r.lua_bytes > 800000, 'size ' .. tostring(r.lua_bytes)).to_be(true)
  end)

  test(name .. ': a reply over the limit is reply_too_large, and the hub stays connected', function()
    local id, pid, state = hub_state()
    expect(state).to_be('connected')
    local keys = fill(name .. '-b', 23, unit, unit_go, 48000) -- about 1.1 MB
    local t0 = os.time()
    local r = measure(keys)
    expect(r.arrived).to_be(false)
    expect(r.code).to_be('reply_too_large')
    expect(r.message).to_match('too large to send')
    expect(r.message).to_match('returned a result')
    expect(r.message).to_match('does not undo changes')
    expect(r.message).to_match('keep its operation key')
    expect(os.time() - t0 <= 3, 'an answer, not a timeout').to_be(true)
    local id2, pid2, state2 = hub_state()
    expect(id2, 'the same hub client').to_be(id)
    expect(pid2, 'the same hub process').to_be(pid)
    expect(state2).to_be('connected')
    expect(alice:call('fix.get', {key = keys[1]}).rev ~= '0', 'the next call works').to_be(true)
  end)
end

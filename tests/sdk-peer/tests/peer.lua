-- app.peer through the real SDK, the real hub and real SQL (docs/plugin-sdk.md "Federation"). The
-- test pulls as the peer "away" (hub.peer.pull acks with after = seq): the peer check is a name
-- check (docs/hub.md "Trust limit").

local alice = agent('alice')

-- pull answers the unacked envelopes of plugin speer to "away" after seq after. A reply holds at
-- most 100 envelopes or 1 MiB ('more'): the next page acks the earlier ones. ack acks up to seq.
local function pull(after)
  local out = {}
  after = after or 0
  while true do
    local r = H.hub('hub.peer.pull', {hub = 'away', after = after})
    for _, e in ipairs(r.envelopes or {}) do
      if e.plugin == 'speer' then out[#out + 1] = e end
      after = math.max(after, e.seq)
    end
    if not r.more then return out end
  end
end
local function ack(seq) H.hub('hub.peer.pull', {hub = 'away', after = seq}) end
local function outbox()
  for _, p in ipairs(H.peer_status().peers) do if p.name == 'away' then return p.outbox end end
end

test('the drain runs after the reply: a committed send stays queued while its handler runs', function()
  local r = alice:call('speer.send_wait', {hub = 'away', k = 'after-reply', text = 'x', s = 2})
  expect(r.state, 'queued 2 s after the commit, in the handler').to_be('queued')
  H.peer_wait('speer', 'away', 'taken', 20)
  local row = sql('SELECT state, hub_seq FROM sdk_peer_outbox WHERE key = ?', {'after-reply'})[1]
  expect(row.state).to_be('sent')
  local envs = pull(0)
  expect(#envs, 'one envelope at the hub').to_be(1)
  expect({envs[1].key, envs[1].body}).to_equal({'after-reply', '{"t":"x"}'})
  -- acked: the peer acked every envelope to it.
  ack(envs[#envs].seq)
  expect(H.peer_wait('speer', 'away', 'acked', 20), 'no queued row now').to_be(0)
  expect(outbox(), 'the hub outbox').to_be(0)
end)

test('peer_wait fails at once on a failed row, with its code', function()
  -- A gate keeps the row queued (a passing error: the peer waits 2 s). Then the next try answers a
  -- final code: peer_wait, called while the row is queued, raises at that try.
  local gate = fault({point = 'action', action = 'hub.peer.send', when = 'before', mode = 'gate', plugin = 'speer'})
  alice:call('speer.send', {hub = 'away', k = 'lost', text = 'x'})
  wait('the gate', function() return fault_fired(gate) end, 20)
  fault({point = 'action', action = 'hub.peer.send', when = 'before', mode = 'fail', fail_code = 'unknown_hub', plugin = 'speer'})
  fault_release(gate)
  local t0 = os.time()
  expect_error(function() H.peer_wait('speer', 'away', 'taken', 60) end, 'peer_failed')
  expect(os.time() - t0 < 15, 'before the timeout').to_be(true)
  local row = sql('SELECT state, code FROM sdk_peer_outbox WHERE key = ?', {'lost'})[1]
  expect({row.state, row.code}).to_equal({'failed', 'unknown_hub'})
  -- A hub that is not a peer raises at the call, not after the timeout (claude-42 L1).
  local ok, e = pcall(H.peer_wait, 'speer', 'nobody', 'taken', 30)
  expect(not ok and tostring(e):find('no peer nobody', 1, true) ~= nil, tostring(e)).to_be(true)
end)

test('a backlog over one SQL reply drains in row order, and status counts it', function()
  -- 22 rows of 50 000 bytes: a query of all bodies is over the hub's reply limit (about 1 040 000).
  local gate = fault({point = 'action', action = 'hub.peer.send', when = 'before', mode = 'gate', plugin = 'speer'})
  local body = string.rep('b', 50000)
  for i = 1, 22 do alice:call('speer.send', {hub = 'away', k = string.format('bulk-%02d', i), text = body}) end
  wait('the gate', function() return fault_fired(gate) end, 20)
  local st = alice:call('speer.status', {}).peers.away
  expect(st.queued, 'queued').to_be(22)
  expect(st.last_error and st.last_error.code, 'the gate is a passing error').to_be('unavailable')
  fault_release(gate)
  expect(H.peer_wait('speer', 'away', 'taken', 150)).to_be(22)
  local seqs = sql("SELECT key, hub_seq FROM sdk_peer_outbox WHERE key LIKE 'bulk-%' ORDER BY n", {})
  for i = 2, #seqs do
    expect(tonumber(seqs[i].hub_seq) > tonumber(seqs[i - 1].hub_seq), 'hub seq order at ' .. seqs[i].key).to_be(true)
  end
  st = alice:call('speer.status', {}).peers.away
  expect(st == nil or st.queued == 0, 'nothing queued').to_be(true)
  local envs = pull(0)
  local bulk = 0
  for _, e in ipairs(envs) do if e.key:match('^bulk%-') then bulk = bulk + 1 end end
  expect(bulk, 'one envelope per row').to_be(22)
  ack(envs[#envs].seq)
end)

-- sdk_text is the SDK's body text for {t = s} (peer.lua encode_string): < > & and U+2028 stay raw.
local SHORT = {['"'] = '\\"', ['\\'] = '\\\\', ['\b'] = '\\b', ['\f'] = '\\f', ['\n'] = '\\n', ['\r'] = '\\r', ['\t'] = '\\t'}
local function sdk_text(s)
  return '{"t":"' .. s:gsub('[%z\1-\31"\\]', function(c) return SHORT[c] or string.format('\\u%04x', c:byte()) end) .. '"}'
end

-- fit answers the longest text of unit (then 'a' bytes) that the SDK accepts for key k to "away":
-- #enc(text) + 2 #enc(hub) + 2 #enc(key) + 1024 <= 65536, where text = sdk_text(s).
local function fit(unit, k)
  local function cost(s)
    return #rex.json.encode(sdk_text(s)) + 2 * #rex.json.encode('away') + 2 * #rex.json.encode(k) + 1024
  end
  -- Each character adds a fixed cost, so the count is a division.
  local base = cost('')
  local s = string.rep(unit, math.floor((65536 - base) / (cost(unit) - base)))
  s = s .. string.rep('a', 65536 - cost(s))
  if cost(s) ~= 65536 then error('fit: ' .. cost(s) .. ' is not the limit for ' .. string.format('%q', unit)) end
  return s
end

test('a body at the SDK size limit reaches the hub whole; one byte more is too_large at the call', function()
  -- The receive side at the limit is TestPeerSDKLab (two hubs): this runner is a Rex Lua script, and
  -- its own request with such a body is over the 64 KiB that a script can send. For the same reason
  -- only units that Rex encodes shorter in the request than in the SDK measure: quotes and newlines.
  -- TestPeerSDKLab sends < > & and U+2028 at the limit too.
  for i, unit in ipairs({'"\\', 'a\nb\r\t'}) do
    local k = 'size-' .. i
    local s = fit(unit, k)
    alice:call('speer.send', {hub = 'away', k = k, text = s})
    expect_error(function() alice:call('speer.send', {hub = 'away', k = k, text = s .. 'a'}) end, 'too_large')
    H.peer_wait('speer', 'away', 'taken', 30)
    local env
    for _, e in ipairs(pull(0)) do if e.key == k then env = e end end
    expect(env ~= nil, 'the hub has the envelope ' .. k).to_be(true)
    expect(env.body == sdk_text(s), 'the stored text of ' .. k).to_be(true)
    ack(env.seq)
  end
end)

test('a hub crash while a row is queued: the plugin keeps it and sends it once after the restart', function()
  local gate = fault({point = 'action', action = 'hub.peer.send', when = 'before', mode = 'gate', plugin = 'speer'})
  alice:call('speer.send', {hub = 'away', k = 'crash-queued', text = 'q'})
  wait('the gate', function() return fault_fired(gate) end, 20)
  expect(sql('SELECT state FROM sdk_peer_outbox WHERE key = ?', {'crash-queued'})[1].state).to_be('queued')
  fault_release(gate)
  hub:restart({crash = true})
  H.peer_wait('speer', 'away', 'taken', 90)
  expect(sql('SELECT state FROM sdk_peer_outbox WHERE key = ?', {'crash-queued'})[1].state).to_be('sent')
  local n = 0
  for _, e in ipairs(pull(0)) do if e.key == 'crash-queued' then n = n + 1 end end
  expect(n, 'one envelope').to_be(1)
end)

test('a hub crash after the take: the envelope stays; a repeat delivery commits once', function()
  alice:call('speer.send', {hub = 'away', k = 'crash-taken', text = 't'})
  H.peer_wait('speer', 'away', 'taken', 20)
  hub:restart({crash = true})
  local env
  for _, e in ipairs(pull(0)) do if e.key == 'crash-taken' then env = e end end
  expect(env ~= nil, 'the envelope is still in the hub outbox').to_be(true)
  for _ = 1, 2 do
    expect(H.peer_deliver({plugin = 'speer', from_hub = 'away', key = env.key, body = env.body}).reply).to_equal({})
  end
  expect(#sql('SELECT key FROM got WHERE key = ?', {'crash-taken'}), 'one listener effect').to_be(1)
  ack(env.seq)
  expect(outbox()).to_be(0)
end)

-- app.peer through the real SDK, the real hub and real SQL (docs/plugin-sdk.md "Federation"). The
-- test pulls as the peer "away" (hub.peer.pull acks with after = seq when the pull names the hub's
-- outbox_id): the peer check is a name check (docs/hub.md "Trust limit").

local alice = agent('alice')

-- outbox_id is the lab hub's outbox identity: a pull acks only when it names it (docs/hub.md
-- "Outbox identity"). A pull without it acks nothing and reads from seq 0.
local function outbox_id() return H.hub('hub.peer.pull', {hub = 'away', after = 0}).outbox_id end

-- pull answers the unacked envelopes of plugin speer to "away" after seq after. A reply holds at
-- most 100 envelopes or 1 MiB ('more'): the next page acks the earlier ones. ack acks up to seq.
local function pull(after)
  local out, id = {}, outbox_id()
  after = after or 0
  while true do
    local r = H.hub('hub.peer.pull', {hub = 'away', after = after, outbox_id = id})
    for _, e in ipairs(r.envelopes or {}) do
      if e.plugin == 'speer' then out[#out + 1] = e end
      after = math.max(after, e.seq)
    end
    if not r.more then return out end
  end
end
local function ack(seq) H.hub('hub.peer.pull', {hub = 'away', after = seq, outbox_id = outbox_id()}) end
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

-- part is the SDK's ID part: "<byte length>:<value>".
local function part(s) return #s .. ':' .. s end

test('real SQLite: the step from peer schema 1 keeps every decision, also with 600 keys of 512 escaped bytes', function()
  -- The digest of {"t":"a"} as this SDK stores it: a version 1 row has the same digest.
  expect(H.peer_deliver({plugin = 'speer', from_hub = 'away', key = 'probe', body = '{"t":"a"}'}).reply).to_equal({})
  local d = sql('SELECT digest FROM sdk_peer_seen WHERE key = ?', {'probe'})[1].digest
  local got0 = #sql('SELECT key FROM got', {})
  -- Stop the plugin, and make the database a version 1 database (the schema of 24e35b0d) with:
  -- an accepted row, a refused row, a queued row, and 600 accepted rows whose keys are 505 bytes of
  -- U+0001 (6 bytes each as JSON) and a 7-digit number: 512 bytes (codex-15).
  H.hub('hub.admin.stop', {plugin = 'speer'})
  for _, s in ipairs({'DROP TABLE sdk_peer_seen', 'DROP TABLE sdk_peer_outbox',
    'CREATE TABLE sdk_peer_outbox (n INTEGER PRIMARY KEY, id TEXT NOT NULL UNIQUE, hub TEXT NOT NULL, key TEXT NOT NULL, '
      .. 'digest TEXT NOT NULL, body TEXT, state TEXT NOT NULL, hub_seq INTEGER, code TEXT, message TEXT, created INTEGER NOT NULL, '
      .. 'sent_at INTEGER, failed_at INTEGER)',
    'CREATE INDEX sdk_peer_outbox_queue ON sdk_peer_outbox (hub, state, n)',
    'CREATE TABLE sdk_peer_seen (id TEXT PRIMARY KEY, from_hub TEXT NOT NULL, key TEXT NOT NULL, digest TEXT NOT NULL, '
      .. 'refused TEXT, message TEXT, at INTEGER NOT NULL)',
    "UPDATE sdk_meta SET version = 1 WHERE part = 'peer'"}) do
    H.sql_write(s, {}, 'speer')
  end
  local seen = 'INSERT INTO sdk_peer_seen (id, from_hub, key, digest, refused, message, at) VALUES (?, ?, ?, ?, ?, ?, 1)'
  H.sql_write(seen, {part('away') .. part('v1-ok'), 'away', 'v1-ok', d, {null = true}, {null = true}}, 'speer')
  H.sql_write(seen, {part('away') .. part('v1-no'), 'away', 'v1-no', d, 'unknown_recipient', 'no such agent'}, 'speer')
  local pad = string.rep('\1', 505)
  H.sql_write("WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 600) "
    .. "INSERT INTO sdk_peer_seen (id, from_hub, key, digest, at) SELECT '4:away512:' || ?1 || printf('%07d', i), 'away', "
    .. "?1 || printf('%07d', i), ?2, 1 FROM n", {pad, d}, 'speer')
  H.sql_write('INSERT INTO sdk_peer_outbox (id, hub, key, digest, body, state, created) VALUES (?, ?, ?, ?, ?, ?, ?)',
    {part('away') .. part('v1-queued'), 'away', 'v1-queued', 'x', '{"t":"q"}', 'queued', os.time()}, 'speer')
  expect(sql('SELECT count(*) AS n FROM sdk_peer_seen', {})[1].n, 'the version 1 rows (a count: their IDs are over one SQL reply)').to_be(602)
  H.hub('hub.admin.start', {plugin = 'speer'})
  wait('speer answers again', function() return pcall(alice.call, alice, 'speer.status', {}) end, 30)
  expect(sql("SELECT version FROM sdk_meta WHERE part = 'peer'", {})[1].version, 'the step ran').to_be(2)
  expect(sql("SELECT count(*) AS n FROM sdk_peer_seen WHERE from_plugin = 'speer'", {})[1].n, 'every seen row has a sender').to_be(602)
  -- The rewritten IDs are the IDs that receive builds: a repeat of each kind is the stored decision.
  local function again(key) return H.peer_deliver({plugin = 'speer', from_hub = 'away', key = key, body = '{"t":"a"}'}).reply end
  expect(again('v1-ok'), 'an accepted key').to_equal({})
  expect(again('v1-no'), 'a refused key').to_equal({refused = 'unknown_recipient', message = 'no such agent'})
  expect(again(pad .. '0000001'), 'a long key').to_equal({})
  expect(again(pad .. '0000600'), 'the last long key').to_equal({})
  expect(#sql('SELECT key FROM got', {}), 'no listener run').to_be(got0)
  local id = sql('SELECT id FROM sdk_peer_seen WHERE key = ?', {'v1-ok'})[1].id
  expect(id).to_be(part('away') .. part('speer') .. part('v1-ok'))
  -- The queued row goes to this plugin: target NULL, and the envelope names speer.
  H.peer_wait('speer', 'away', 'taken', 30)
  expect(sql('SELECT target FROM sdk_peer_outbox WHERE key = ?', {'v1-queued'})[1].target).to_be(nil)
  local env
  for _, e in ipairs(pull(0)) do if e.key == 'v1-queued' then env = e end end
  expect({env and env.key, env and env.body}).to_equal({'v1-queued', '{"t":"q"}'})
  ack(env.seq)
end)

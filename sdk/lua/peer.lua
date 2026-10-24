-- Federation for SDK plugins (docs/plugin-sdk.md "Federation"; the hub side is docs/hub.md "Peer
-- hubs"). The interface is the hub lead's lead-sdkfed3, lead-sdkfed4 and lead-sdkfed6. lazyrex.lua
-- loads this file in its own environment and gives it the SDK parts that it needs (k).
--
-- Send: peer.send writes a row of sdk_peer_outbox in the caller's transaction. After that
-- transaction commits, the drain gives each queued row to hub.peer.send: one call at a time per
-- peer, in row order, with the key and the body text of the row. So the hub assigns its seq in
-- row order, and the receiver delivers in that order.
-- Receive: the hub calls peer.receive. The listener and its dedupe row in sdk_peer_seen commit in
-- one transaction, so its database effects happen once for each (origin hub, key).
return function(k)
  local db, run, frame, hub, fail, part, digest, normalize = k.db, k.run, k.frame, k.hub, k.fail, k.part, k.digest, k.normalize
  local P = {}

  local VERSION = 1 -- sdk_meta part "peer"
  local MAX_BODY = 65536 -- the hub's limit, measured as Go's json.Marshal of the body text
  local PRUNE_S = 30 * 86400 -- sent rows older than this go at start; seen rows stay (lead-sdkfed8)
  local SLOW_S = 2 -- a receive listener that runs longer is logged
  -- The final answers of hub.peer.send (claude-43, peers.go on main): an untyped bad() (a bad key,
  -- a body that is not a string) answers refused. Any other error, also a timeout, is passing.
  local FINAL = {operation_conflict = true, not_plugin = true, unknown_hub = true, too_large = true, refused = true}
  local TABLES = {
    sdk_meta = 'CREATE TABLE sdk_meta (part TEXT PRIMARY KEY, version INTEGER NOT NULL)',
    sdk_peer_outbox = 'CREATE TABLE sdk_peer_outbox (n INTEGER PRIMARY KEY, id TEXT NOT NULL UNIQUE, hub TEXT NOT NULL, key TEXT NOT NULL, '
      .. 'digest TEXT NOT NULL, body TEXT, state TEXT NOT NULL, hub_seq INTEGER, code TEXT, message TEXT, created INTEGER NOT NULL, '
      .. 'sent_at INTEGER, failed_at INTEGER)',
    sdk_peer_seen = 'CREATE TABLE sdk_peer_seen (id TEXT PRIMARY KEY, from_hub TEXT NOT NULL, key TEXT NOT NULL, digest TEXT NOT NULL, '
      .. 'refused TEXT, message TEXT, at INTEGER NOT NULL)',
  }
  -- The drain and the status read the outbox per peer and state, in row order.
  local QUEUE_INDEX = 'CREATE INDEX sdk_peer_outbox_queue ON sdk_peer_outbox (hub, state, n)'
  P.TABLES = TABLES

  local function now() return os.time() end
  local function hub_name(s) return type(s) == 'string' and #s <= 63 and s:match('^[a-z0-9][a-z0-9%-]*$') ~= nil end

  -- JSON ----------------------------------------------------------------------------------------

  -- valid_utf8 answers whether s is valid UTF-8 (no overlong form, no surrogate, at most U+10FFFF).
  local function valid_utf8(s)
    local i, n = 1, #s
    while i <= n do
      local c = s:byte(i)
      local need, lo, hi = 0, 0x80, 0xBF
      if c < 0x80 then need = 0
      elseif c >= 0xC2 and c <= 0xDF then need = 1
      elseif c >= 0xE0 and c <= 0xEF then need = 2; if c == 0xE0 then lo = 0xA0 elseif c == 0xED then hi = 0x9F end
      elseif c >= 0xF0 and c <= 0xF4 then need = 3; if c == 0xF0 then lo = 0x90 elseif c == 0xF4 then hi = 0x8F end
      else return false end
      for j = 1, need do
        local b = s:byte(i + j)
        if not b then return false end
        if j == 1 then if b < lo or b > hi then return false end
        elseif b < 0x80 or b > 0xBF then return false end
      end
      i = i + need + 1
    end
    return true
  end

  local SHORT = {['"'] = '\\"', ['\\'] = '\\\\', ['\b'] = '\\b', ['\f'] = '\\f', ['\n'] = '\\n', ['\r'] = '\\r', ['\t'] = '\\t'}
  local function encode_string(s)
    if not valid_utf8(s) then fail('invalid_input', 'peer.send: a string of the body is not valid UTF-8') end
    return '"' .. s:gsub('[%z\1-\31"\\]', function(c) return SHORT[c] or string.format('\\u%04x', c:byte()) end) .. '"'
  end

  -- encode gives the canonical JSON text of v: object members sorted by name, so two equal tables
  -- give the same text. A table with the keys 1 to n (n > 0) is an array; a table with string keys,
  -- or an empty table, is an object. nil cannot be a value: a nil field is absent; false stays false.
  local function encode(v, out, depth)
    if depth > 64 then fail('invalid_input', 'peer.send: the body is nested more than 64 levels') end
    local t = type(v)
    if t == 'string' then out[#out + 1] = encode_string(v)
    elseif t == 'boolean' then out[#out + 1] = v and 'true' or 'false'
    elseif t == 'number' then
      if v ~= v or v == math.huge or v == -math.huge then fail('invalid_input', 'peer.send: a number of the body is not finite') end
      if v % 1 == 0 and v >= -2 ^ 53 and v <= 2 ^ 53 then out[#out + 1] = string.format('%.0f', v)
      else out[#out + 1] = string.format('%.17g', v) end
    elseif t == 'table' then
      local count, strings = 0, {}
      for key in pairs(v) do
        count = count + 1
        if type(key) == 'string' then strings[#strings + 1] = key
        elseif type(key) ~= 'number' then fail('invalid_input', 'peer.send: a table key of the body is a ' .. type(key)) end
      end
      if #strings == count then
        table.sort(strings)
        out[#out + 1] = '{'
        for i, key in ipairs(strings) do
          if i > 1 then out[#out + 1] = ',' end
          out[#out + 1] = encode_string(key)
          out[#out + 1] = ':'
          encode(v[key], out, depth + 1)
        end
        out[#out + 1] = '}'
      elseif #strings == 0 and #v == count then
        out[#out + 1] = '['
        for i = 1, count do
          if i > 1 then out[#out + 1] = ',' end
          encode(v[i], out, depth + 1)
        end
        out[#out + 1] = ']'
      else
        fail('invalid_input', 'peer.send: a table of the body is neither an array (keys 1 to n) nor an object (string keys)')
      end
    else
      fail('invalid_input', 'peer.send: a value of the body is a ' .. t)
    end
  end
  local function encode_body(body)
    if type(body) ~= 'table' then fail('invalid_input', 'peer.send: the body must be a table (a JSON object), not a ' .. type(body)) end
    local out = {}
    encode(body, out, 1)
    if out[1] ~= '{' then fail('invalid_input', 'peer.send: the body must be a JSON object, not an array') end
    return k.concat(out)
  end
  P.encode_body = encode_body

  -- measure gives the length of Go's json.Marshal of text (a Go string), as the hub measures the
  -- body (lead-sdkfed4 2): 2 quotes; \" and \\ give 2 bytes; \b \f \n \r \t give 2 (Go 1.22 and
  -- later); another control character, <, > and & give 6 (\u00XX, HTML escaping); U+2028 and U+2029
  -- give 6; an invalid UTF-8 byte gives 3 (U+FFFD); DEL and any other character keep their bytes.
  -- Measured with Go 1.27.2 json.Marshal (claude-37); docs/hub.md "Size" pins the values.
  local function measure(text)
    local n, i, len = 2, 1, #text
    while i <= len do
      local c = text:byte(i)
      if c < 0x80 then
        if c == 0x22 or c == 0x5C or c == 0x08 or c == 0x0C or c == 0x0A or c == 0x0D or c == 0x09 then n = n + 2
        elseif c < 0x20 or c == 0x3C or c == 0x3E or c == 0x26 then n = n + 6
        else n = n + 1 end
        i = i + 1
      else
        local size = c >= 0xF0 and 4 or c >= 0xE0 and 3 or c >= 0xC0 and 2 or 1
        local s = text:sub(i, i + size - 1)
        if size == 1 or #s < size or not valid_utf8(s) then n, i = n + 3, i + 1
        elseif s == '\226\128\168' or s == '\226\128\169' then n, i = n + 6, i + 3
        else n, i = n + size, i + size end
      end
    end
    return n
  end
  P.measure = measure

  -- IDs: part() is "<byte length>:<value>", so a pair of parts has one reading.
  local function parts(s)
    local out, i = {}, 1
    while i <= #s do
      local len, rest = s:match('^(%d+):()', i)
      if not len then return nil end
      len = tonumber(len)
      if rest + len - 1 > #s then return nil end
      out[#out + 1] = s:sub(rest, rest + len - 1)
      i = rest + len
    end
    return out
  end

  -- Schema --------------------------------------------------------------------------------------

  -- setup makes the SDK tables (before the plugin migrations, in its own transaction) and deletes
  -- old sent rows, never a seen row (lead-sdkfed8). A database with a newer peer schema fails the
  -- start.
  function P.setup()
    run({tx = true}, function()
      local have = {}
      for _, r in ipairs(db.query("SELECT name FROM sqlite_master WHERE type = 'table'")) do have[r.name] = true end
      if not have.sdk_meta then db.exec(TABLES.sdk_meta) end
      local row = db.first('SELECT version FROM sdk_meta WHERE part = ?', 'peer')
      local v = row and tonumber(row.version) or 0
      if v > VERSION then
        fail('internal', 'The database has SDK peer schema ' .. v .. '; this SDK knows ' .. VERSION .. '. Update the plugin or delete its database.')
      end
      if v == 0 then
        if not have.sdk_peer_outbox then db.exec(TABLES.sdk_peer_outbox); db.exec(QUEUE_INDEX) end
        if not have.sdk_peer_seen then db.exec(TABLES.sdk_peer_seen) end
        db.exec('INSERT INTO sdk_meta (part, version) VALUES (?, ?)', 'peer', VERSION)
      end
      db.exec('DELETE FROM sdk_peer_outbox WHERE sent_at < ?', now() - PRUNE_S)
    end)
  end

  -- Drain ---------------------------------------------------------------------------------------

  -- D: the drain state of this process. peers[hub] = {wait, next, error}: the backoff of a peer
  -- after a passing error, and that error.
  -- due: the time of the earliest armed timer. A later trigger with a sooner time arms one more
  -- timer; an extra drain finds nothing to do.
  local D = {running = false, again = false, due = nil, peers = {}}
  local spec = {}
  local drain

  local function arm(delay)
    local at = now() + delay
    if D.due and D.due <= at then return end
    D.due = at
    rex.after(delay, function()
      if D.due == at then D.due = nil end
      drain()
    end)
  end
  P.arm = arm

  local function mark_sent(row, seq)
    run({tx = true}, function()
      db.exec('UPDATE sdk_peer_outbox SET state = ?, hub_seq = ?, body = ?, sent_at = ? WHERE id = ?', 'sent', seq, nil, now(), row.id)
    end)
  end

  -- mark_failed makes the row failed and runs the failed listener in the same transaction. A
  -- listener error is logged, and the row becomes failed without it.
  local function mark_failed(row, e)
    local at = now()
    local function mark()
      db.exec('UPDATE sdk_peer_outbox SET state = ?, code = ?, message = ?, failed_at = ? WHERE id = ?', 'failed', e.code, e.message, at, row.id)
    end
    if spec.failed then
      local ok, err = pcall(run, {tx = true}, function()
        mark()
        spec.failed({hub = row.hub, key = row.key, code = e.code, message = e.message, at = at})
      end)
      if ok then return end
      print('lazyrex: peer: the failed listener of ' .. row.key .. ' to ' .. row.hub .. ' failed: ' .. normalize(err).message)
    end
    run({tx = true}, mark)
  end

  local function drain_once()
    -- One row per peer, then one row at a time: a reply holds at most one body, so a backlog of any
    -- size drains (one query of all queued bodies fails with too_large over one reply).
    local soonest
    for _, p in ipairs(db.query('SELECT hub, MIN(n) AS first FROM sdk_peer_outbox WHERE state = ? GROUP BY hub ORDER BY first', 'queued')) do
      local h = p.hub
      local st = D.peers[h] or {wait = 0, next = 0}
      D.peers[h] = st
      if st.next > now() then
        soonest = math.min(soonest or math.huge, st.next - now())
      else
        local last = 0
        while true do
          local r = db.first('SELECT n, id, hub, key, body FROM sdk_peer_outbox WHERE hub = ? AND state = ? AND n > ? ORDER BY n LIMIT 1', h, 'queued', last)
          if not r then break end
          last = tonumber(r.n)
          local ok, res = pcall(hub, 'hub.peer.send', {hub = r.hub, key = r.key, body = r.body})
          if ok then
            mark_sent(r, type(res) == 'table' and res.seq or nil)
            st.wait, st.error = 0, nil
          else
            local e = normalize(res)
            if FINAL[e.code] then
              print('lazyrex: peer: ' .. r.key .. ' to ' .. h .. ' cannot go (' .. e.code .. '): ' .. tostring(e.message))
              mark_failed(r, e)
            else
              st.wait = math.min(st.wait == 0 and 2 or st.wait * 2, 60)
              st.next, st.error = now() + st.wait, {code = e.code, message = e.message, at = now()}
              print('lazyrex: peer: hub.peer.send of ' .. r.key .. ' to ' .. h .. ' failed; it waits ' .. st.wait .. ' s: ' .. tostring(e.message))
              soonest = math.min(soonest or math.huge, st.wait)
              break
            end
          end
        end
      end
    end
    if soonest then arm(math.max(soonest, 1)) end
  end

  -- drain runs outside any frame: each SQL call and each hub.peer.send stands alone. One drain runs
  -- at a time; a trigger during a drain runs one more drain after it.
  function drain()
    if D.running then D.again = true; return end
    D.running = true
    repeat
      D.again = false
      local ok, err = pcall(drain_once)
      if not ok then print('lazyrex: peer: the outbox drain failed: ' .. normalize(err).message); arm(5) end
    until not D.again
    D.running = false
  end
  P.drain = drain

  -- committed: a frame that queued a row committed. The drain runs after the reply (rex.after 0).
  function P.committed() arm(0) end

  -- Receive -------------------------------------------------------------------------------------

  local REFUSAL = {} -- the marker of a refusal raised out of the listener's transaction

  local function refusal(code, message)
    if type(code) ~= 'string' or code == '' then fail('internal', 'peer.refuse: the code must be a nonempty string, not ' .. type(code)) end
    if message ~= nil and type(message) ~= 'string' then fail('internal', 'peer.refuse: the message must be a string, not ' .. type(message)) end
    return {refused = code, message = message}
  end

  -- receive answers peer.receive {from_hub, key, body}. A malformed envelope is refused before the
  -- listener: the same envelope gives the same refusal, so it is not stored.
  function P.receive(a)
    local from_hub, key, text = a.from_hub, a.key, a.body
    if not hub_name(from_hub) then return refusal('invalid_envelope', 'from_hub is not a hub name') end
    if type(key) ~= 'string' or key == '' then return refusal('invalid_envelope', 'The key is not a nonempty string') end
    if type(text) ~= 'string' or not text:match('^%s*{') then return refusal('invalid_envelope', 'The body is not a JSON object') end
    local value, err = rex.json.decode(text)
    if err ~= nil or type(value) ~= 'table' then return refusal('invalid_envelope', 'The body is not a JSON object') end
    local id, d = part(from_hub) .. part(key), digest(text)
    local started = now()
    local ok, res = pcall(run, {tx = true}, function()
      local seen = db.first('SELECT digest, refused, message FROM sdk_peer_seen WHERE id = ?', id)
      if seen then
        if seen.digest ~= d then return refusal('operation_conflict', 'The key ' .. key .. ' from ' .. from_hub .. ' came with another body') end
        if seen.refused then return {refused = seen.refused, message = seen.message} end
        return {}
      end
      local r = spec.receive({from_hub = from_hub, key = key, body = value})
      if r ~= nil and type(r) ~= 'table' then fail('internal', 'The peer receive listener must return nil, a table or peer.refuse(...), not a ' .. type(r)) end
      if r and r.refused ~= nil then
        local checked = refusal(r.refused, r.message)
        error({[REFUSAL] = true, refused = checked.refused, message = checked.message}, 0)
      end
      db.exec('INSERT INTO sdk_peer_seen (id, from_hub, key, digest, at) VALUES (?, ?, ?, ?, ?)', id, from_hub, key, d, now())
      return {}
    end)
    local took = now() - started
    if took > SLOW_S then print(string.format('lazyrex: peer: the receive listener ran %d s for %s from %s; a slow listener delays every envelope from that hub', took, key, from_hub)) end
    if ok then return res end
    if type(res) == 'table' and res[REFUSAL] then
      -- A refusal rolls back the listener's writes; the dedupe row keeps the refusal.
      run({tx = true}, function()
        if not db.first('SELECT digest FROM sdk_peer_seen WHERE id = ?', id) then
          db.exec('INSERT INTO sdk_peer_seen (id, from_hub, key, digest, refused, message, at) VALUES (?, ?, ?, ?, ?, ?, ?)', id, from_hub, key, d, res.refused, res.message, now())
        end
      end)
      return {refused = res.refused, message = res.message}
    end
    print('lazyrex: peer: THE RECEIVE LISTENER FAILED for ' .. key .. ' from ' .. from_hub .. ': ' .. normalize(res).message
      .. '. The hub gives it again for at most 5 minutes, and every later envelope from that hub waits. Refuse a permanent problem with peer.refuse.')
    error(res, 0)
  end

  -- API -----------------------------------------------------------------------------------------

  -- declare is app.peer{receive = fn, failed = fn}: once, at load.
  function P.declare(s)
    s = s or {}
    if type(s) ~= 'table' then error('app.peer takes a table', 3) end
    for name in pairs(s) do
      if name ~= 'receive' and name ~= 'failed' then error('app.peer: unknown field ' .. tostring(name), 3) end
    end
    for _, name in ipairs({'receive', 'failed'}) do
      if s[name] ~= nil and type(s[name]) ~= 'function' then error('app.peer: ' .. name .. ' must be a function', 3) end
    end
    spec = s
    local api = {}

    local function in_tx(name)
      local f = frame()
      if not (f and f.tx) or f.in_render then
        fail('invalid_input', name .. ' runs in a write transaction: an action, a view handler, a callback or app.db.transaction')
      end
      return f
    end
    local function state_of(row, key)
      local out = {key = key, state = row.state}
      if row.state == 'sent' then out.hub_seq = row.hub_seq end
      if row.state == 'failed' then out.code, out.message = row.code, row.message end
      return out
    end

    -- send(hub, key, body): stages a row in the caller's transaction. The row is durable when that
    -- transaction commits; a rollback leaves no row and sends nothing.
    function api.send(to_hub, key, body)
      local f = in_tx('peer.send')
      if not hub_name(to_hub) then fail('invalid_input', 'peer.send: the hub must be a hub name ([a-z0-9][a-z0-9-], at most 63 bytes)') end
      if type(key) ~= 'string' or #key < 1 or #key > 512 then fail('invalid_input', 'peer.send: the key must be a string of 1 to 512 bytes') end
      local text = encode_body(body)
      local size = measure(text)
      if size > MAX_BODY then fail('too_large', 'peer.send: the body is ' .. size .. ' bytes as the hub measures it; the limit is ' .. MAX_BODY) end
      -- The row insert must fit one Rex request (k.MAX_REQUEST): the body, the hub and the key as Rex
      -- encodes them (the hub and the key twice: the row ID has them too), and 1024 bytes for the
      -- SQL text and the framing. The hub.peer.send request of the row is smaller than its insert.
      local wire = #rex.json.encode(text)
      if wire + 2 * #rex.json.encode(to_hub) + 2 * #rex.json.encode(key) + 1024 > k.MAX_REQUEST then
        fail('too_large', 'peer.send: the body is ' .. wire .. ' bytes as Rex sends it; with the key and the row it does not fit one Rex request ('
          .. k.MAX_REQUEST .. ' bytes)')
      end
      local id, d = part(to_hub) .. part(key), digest(text)
      local row = db.first('SELECT state, digest, hub_seq, code, message FROM sdk_peer_outbox WHERE id = ?', id)
      if row then
        if row.digest ~= d then fail('operation_conflict', 'peer.send: the key ' .. key .. ' to ' .. to_hub .. ' has another body') end
        return state_of(row, key)
      end
      db.exec('INSERT INTO sdk_peer_outbox (id, hub, key, digest, body, state, created) VALUES (?, ?, ?, ?, ?, ?, ?)', id, to_hub, key, d, text, 'queued', now())
      f.peer_queued = true
      return {key = key, state = 'queued'}
    end

    -- retry(hub, key): a failed row becomes queued again, with the same body text.
    function api.retry(to_hub, key)
      local f = in_tx('peer.retry')
      local id = part(tostring(to_hub)) .. part(tostring(key))
      local row = db.first('SELECT state FROM sdk_peer_outbox WHERE id = ?', id)
      if not row or row.state ~= 'failed' then fail('invalid_input', 'peer.retry: no failed row for ' .. tostring(key) .. ' to ' .. tostring(to_hub)) end
      db.exec('UPDATE sdk_peer_outbox SET state = ?, code = ?, message = ?, failed_at = ? WHERE id = ?', 'queued', nil, nil, nil, id)
      D.peers[to_hub] = nil
      f.peer_queued = true
      return {key = key, state = 'queued'}
    end

    api.refuse = refusal

    -- status: the rows that the hub has not taken (queued) and the failed rows, per hub. last_error
    -- is the last passing error of this process since the peer's last success.
    function api.status()
      local peers, t = {}, now()
      local function of(h) peers[h] = peers[h] or {queued = 0, failed = 0}; return peers[h] end
      -- SQL counts (one row per peer): the status of a backlog of any size fits in one reply.
      for _, r in ipairs(db.query('SELECT hub, COUNT(*) AS queued, MIN(created) AS oldest FROM sdk_peer_outbox WHERE state = ? GROUP BY hub', 'queued')) do
        local p = of(r.hub)
        p.queued, p.oldest_queued_age_s = tonumber(r.queued), t - tonumber(r.oldest)
      end
      for _, r in ipairs(db.query('SELECT hub, COUNT(*) AS failed FROM sdk_peer_outbox WHERE state = ? GROUP BY hub', 'failed')) do
        local p = of(r.hub)
        p.failed = tonumber(r.failed)
        local l = db.first('SELECT code, message, failed_at FROM sdk_peer_outbox WHERE hub = ? AND state = ? ORDER BY failed_at DESC, n DESC LIMIT 1', r.hub, 'failed')
        p.last_failed = {code = l.code, message = l.message, at = tonumber(l.failed_at)}
      end
      for h, st in pairs(D.peers) do
        if st.error and peers[h] then peers[h].last_error, peers[h].next_try_s = st.error, math.max(st.next - t, 0) end
      end
      return {peers = peers}
    end

    -- Addresses (Messaging's forms).
    function api.split(id)
      if type(id) ~= 'string' then return nil end
      local l, h = id:match('^(.+)@([a-z0-9][a-z0-9%-]*)$')
      if l and hub_name(h) then return l, h end
      return nil
    end
    function api.join(id, h)
      if not hub_name(h) then fail('invalid_input', 'peer.join: not a hub name: ' .. tostring(h)) end
      return tostring(id) .. '@' .. h
    end
    function api.sender(msg, id) return api.join(id, msg.from_hub) end
    function api.received_id(from_hub, id) return 'peer/' .. part(from_hub) .. part(id) end
    function api.origin_id(local_id, h)
      if type(local_id) ~= 'string' or local_id:sub(1, 5) ~= 'peer/' then return nil end
      local p = parts(local_id:sub(6))
      if p and #p == 2 and p[1] == h then return p[2] end
      return nil
    end
    return api
  end

  function P.receives() return spec.receive ~= nil end
  return P
end

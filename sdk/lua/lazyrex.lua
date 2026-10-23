-- LazyRex Plugin SDK v0 (docs/plugin-sdk.md). A plugin writes:
--   local app = require('lazyrex').plugin('todo')
-- and declares its schema, actions and panels. The SDK does the registration, the hub calls, one
-- SQL transaction for each handler, saved replies, caller checks and error replies.
--
-- Wire (hub.sql handoff v1, hub lead, 2026-10-22; docs/sql.md): hub.sql.begin/exec/query/batch/
-- commit/rollback/replay through client.perform to the hub. NULL is the tag {null = true} in params
-- and in rows: Rex Lua drops a JSON null in an array and shifts the later values (observed on Rex
-- 0.0.1792158932-g88fd41, lab, 2026-10-22: rex.json.decode('[1,null,3]') gave {1, 3}).
--
-- Mutable state is kept in fields of the table S, not in reassigned upvalues: after a caught error,
-- a closure can write a detached copy of a captured local (LazyRex docs/rex-protocol.md, "Rex Lua
-- runtime").
local M = {}
local S = {app = nil, frame = nil}

local MAX_REQUEST = 64 * 1024 -- Rex refuses a larger client.perform request (observed, docs/sql.md section 6).
local TWO53 = 9007199254740992

-- fail raises a structured error. The SDK replies with its code and message.
function M.fail(code, message) error({code = code, message = message or code}, 0) end
local fail = M.fail

-- normalize turns any raised value into {code, message}. error('text') gives code 'failed' and the
-- text without the "file:line: " prefix that Lua adds.
local function normalize(e)
  if type(e) == 'table' and type(e.code) == 'string' then return {code = e.code, message = tostring(e.message or e.code)} end
  local text = tostring(e)
  text = text:gsub('^[^\n]-:%d+: ', '', 1)
  return {code = 'failed', message = text}
end
M.normalize = normalize

-- Values ------------------------------------------------------------------------------------------

-- concat is table.concat with no length limit. Rex Lua fails table.concat of 2560 items or more
-- with "registry overflow" (observed on Rex 0.0.1792158932-g88fd41, lab, 2026-10-22;
-- docs/rex-protocol.md, "Rex Lua runtime"). The limit is lower when more of the stack is in use
-- (2000 items passed and 2559 failed in the SDK test runner). concat joins ranges of at most 500
-- items, then the blocks. It gives the same result and the same errors (a bad item names its own
-- index). boot.lua sets table.concat to it, so plugin code gets it too.
-- raw.concat is called as a field, so that an error names "concat" as table.concat does.
local raw = {concat = table.concat}
local function concat(list, sep, i, j)
  if type(list) ~= 'table' then return raw.concat(list, sep, i, j) end
  -- Only nil takes the default: table.concat refuses false and other values.
  if i == nil then i = 1 end
  if j == nil then j = #list end
  -- A short range, or an odd argument (raw.concat gives its error).
  if type(i) ~= 'number' or type(j) ~= 'number' or i % 1 ~= 0 or j % 1 ~= 0 or j - i < 500 then
    return raw.concat(list, sep, i, j)
  end
  local blocks = {}
  for a = i, j, 500 do blocks[#blocks + 1] = raw.concat(list, sep, a, math.min(a + 499, j)) end
  return concat(blocks, sep)
end
M.concat = concat

-- External reads in a render ----------------------------------------------------------------------

-- Read tracking is the default (user decision, 2026-10-22): the hub renders a view again only after a
-- write to a table that its render read. A render that reads state outside SQL replies refresh = "all",
-- and the hub then renders it after each commit. The SDK replaces each function that reads outside
-- state once, when it loads; boot.lua loads the SDK before the plugin, so plugin code sees only the
-- wrappers. A wrapper marks the open render frame (a table field), then calls the original as a tail
-- call: values and errors pass unchanged, with no protected call in plugin code. The SDK transport
-- uses the originals in raw, so its own calls mark nothing. Not detected: a file or exec handle made
-- before the render, os.time, os.getenv, and native modules (docs/plugin-sdk.md).
local function external()
  local f = S.frame
  if f and f.in_render and f.state then f.state.external = true end
end
local function marked(fn) return function(...) external(); return fn(...) end end
-- api(t) wraps the calls of a Rex API table in place: rex itself, or a table of rex.server(label).
-- A member that the table does not have is left out.
local function api(t)
  for _, k in ipairs({'call', 'invoke', 'wait', 'servers', 'host'}) do
    if type(t[k]) == 'function' then t[k] = marked(t[k]) end
  end
  if type(t.block) == 'table' and type(t.block.call) == 'function' then t.block.call = marked(t.block.call) end
  if type(t.session) == 'table' then
    local session = t.session
    t.session = setmetatable({}, {__index = function(_, k)
      local v = session[k]
      if type(v) == 'function' then return marked(v) end
      return v
    end})
  end
  -- exec is a callable table (observed on Rex 0.0.1792158932-g88fd41): the copy keeps its members.
  if t.exec ~= nil then
    local exec, copy = t.exec, {}
    for k, v in pairs(exec) do copy[k] = type(v) == 'function' and marked(v) or v end
    t.exec = setmetatable(copy, {__call = function(_, ...) external(); return exec(...) end})
  end
  -- A server table gets wrapped calls too, so a call through it marks a render also when the table
  -- was made before the render. All arguments and results pass unchanged.
  if type(t.server) == 'function' then
    local server = t.server
    local function wrapped(r, ...)
      if type(r) == 'table' then api(r) end
      return r, ...
    end
    t.server = function(...) external(); return wrapped(server(...)) end
  end
end
-- install answers true when every replacement holds (it reads each one back).
local function install()
  local function current()
    return {call = rex.call, invoke = rex.invoke, wait = rex.wait, servers = rex.servers, host = rex.host,
      block = type(rex.block) == 'table' and rex.block.call or nil, session = rex.session, exec = rex.exec, server = rex.server,
      open = io.open, lines = io.lines, popen = io.popen, execute = os.execute}
  end
  local before = current()
  raw.call = rex.call
  api(rex)
  for _, k in ipairs({'open', 'lines', 'popen'}) do if type(io[k]) == 'function' then io[k] = marked(io[k]) end end
  if type(os.execute) == 'function' then os.execute = marked(os.execute) end
  local now = current()
  for k, v in pairs(before) do if now[k] == v then return false end end
  return true
end
do
  local ok, held = pcall(install)
  raw.call = raw.call or rex.call
  -- Fail closed: if a replacement did not hold, the SDK tracks no render.
  if not (ok and held) then
    S.untracked = true
    print('lazyrex: the SDK cannot detect reads outside SQL; every view renders after each commit')
  end
end

local NULL = {null = true}
local b64 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/'
local function base64(s)
  local out = {}
  for i = 1, #s, 3 do
    local a, b, c = s:byte(i, i + 2)
    local n = a * 65536 + (b or 0) * 256 + (c or 0)
    local q = {math.floor(n / 262144) % 64, math.floor(n / 4096) % 64, math.floor(n / 64) % 64, n % 64}
    out[#out + 1] = b64:sub(q[1] + 1, q[1] + 1) .. b64:sub(q[2] + 1, q[2] + 1)
      .. (b and b64:sub(q[3] + 1, q[3] + 1) or '=') .. (c and b64:sub(q[4] + 1, q[4] + 1) or '=')
  end
  return concat(out)
end
local unb64 = {}
for i = 1, 64 do unb64[b64:byte(i)] = i - 1 end
local function unbase64(s)
  s = s:gsub('[^%w%+/]', '')
  local out = {}
  for i = 1, #s, 4 do
    local q = {unb64[s:byte(i)] or 0, unb64[s:byte(i + 1)] or 0, unb64[s:byte(i + 2)] or 0, unb64[s:byte(i + 3)] or 0}
    local n = q[1] * 262144 + q[2] * 4096 + q[3] * 64 + q[4]
    local left = #s - i + 1
    out[#out + 1] = string.char(math.floor(n / 65536) % 256)
    if left > 2 then out[#out + 1] = string.char(math.floor(n / 256) % 256) end
    if left > 3 then out[#out + 1] = string.char(n % 256) end
  end
  return concat(out)
end
M.base64, M.unbase64 = base64, unbase64

-- M.int(decimal) is an exact 64-bit integer parameter. M.blob(bytes) is a BLOB parameter.
function M.int(decimal) return {int = tostring(decimal)} end
function M.blob(bytes) return {blob = base64(bytes)} end

local function encode_value(v)
  if v == nil then return NULL end
  local t = type(v)
  if t == 'number' then
    if v ~= v or v == math.huge or v == -math.huge then fail('invalid_input', 'A parameter is not a finite number') end
    if v % 1 == 0 and (v > TWO53 or v < -TWO53) then fail('invalid_input', 'An integer parameter above 2^53 is not exact in Lua; give it as lazyrex.int("<decimal>")') end
    return v
  end
  if t == 'string' or t == 'boolean' then return v end
  if t == 'table' and (v.int ~= nil or v.blob ~= nil or v.null == true) then return v end
  fail('invalid_input', 'A parameter must be nil, a boolean, a number, a string, lazyrex.int or lazyrex.blob')
end
-- params encodes the values after the SQL text. A nil becomes the NULL tag, so the array has no holes.
local function params(n, ...)
  local p = {}
  for i = 1, n do p[i] = encode_value((select(i, ...))) end
  return p
end
local function decode_value(v)
  if type(v) ~= 'table' then return v end
  if v.null == true then return nil end
  if v.int ~= nil then return tostring(v.int) end
  if v.blob ~= nil then return unbase64(v.blob) end
  return v
end
-- rows keys each row by column name. A NULL is nil: a row can have holes.
local function rows_of(reply)
  local columns, out = reply.columns or {}, {}
  for r, raw in ipairs(reply.rows or {}) do
    local row = {}
    for i, name in ipairs(columns) do row[name] = decode_value(raw[i]) end
    out[r] = row
  end
  return out
end

-- Canonical text and digest ----------------------------------------------------------------------

-- canonical answers one text for a value: keys sorted, types marked. The default fingerprint is a
-- digest of it.
local function canonical(v, out)
  local t = type(v)
  if t == 'table' then
    local keys = {}
    for k in pairs(v) do keys[#keys + 1] = k end
    table.sort(keys, function(a, b) if type(a) ~= type(b) then return type(a) < type(b) end return tostring(a) < tostring(b) end)
    out[#out + 1] = '{'
    for _, k in ipairs(keys) do
      -- The key type is part of the text: {'x'} (an array) is not {['1'] = 'x'} (an object).
      out[#out + 1] = type(k):sub(1, 1) .. #tostring(k) .. ':' .. tostring(k) .. '='
      canonical(v[k], out)
      out[#out + 1] = ';'
    end
    out[#out + 1] = '}'
  elseif t == 'string' then
    out[#out + 1] = 's' .. #v .. ':' .. v
  else
    out[#out + 1] = t:sub(1, 1) .. tostring(v)
  end
  return out
end
-- digest answers a non-cryptographic 93-bit digest (three polynomial hashes) of text. A fingerprint
-- only tells a retry with the same content from a new call with the same key of the same caller.
local function digest(text)
  local h = {0, 0, 0}
  local m, p = {2147483629, 2147483587, 2147483579}, {131, 257, 65599}
  for i = 1, #text do
    local c = text:byte(i)
    for j = 1, 3 do h[j] = (h[j] * p[j] + c) % m[j] end
  end
  return string.format('%08x%08x%08x-%d', h[1], h[2], h[3], #text)
end
function M.fingerprint(v) return 'v1/' .. digest(concat(canonical(v, {}))) end

-- Hub calls --------------------------------------------------------------------------------------

-- rex_error raises a Rex or hub error that came as text. The Rex text ends with
-- "(status=<n> code=<code>)" (observed on Rex 0.0.1792158932-g88fd41). A hub.sql error message
-- starts with "<code>: " (handoff v1, C). Only an error with no code, or with unavailable,
-- disconnected or timeout, is unavailable: the outcome is unknown, and the caller can retry.
-- LOST: the Rex codes of a call whose outcome is unknown (a timeout, a dropped connection). A caller
-- retries an unavailable call with the same key.
local LOST = {timeout = true, disconnected = true, client_unavailable = true}
local function rex_error(err, default)
  if type(err) == 'table' then
    if err.code == nil or LOST[err.code] then fail('unavailable', err.message or default) end
    fail(err.code, err.message or default)
  end
  local text = tostring(err or default)
  local code = text:match('code=([%w_]+)%)$')
  local message = text:match('failed: (.-) %(status=%d+ code=[%w_]+%)$') or text
  local inner, rest = message:match('^([%l_]+): (.*)$')
  -- The hub never sends client_unavailable or disconnected, so those stay Rex codes.
  if inner and inner ~= 'client_unavailable' and inner ~= 'disconnected' and (code == nil or code == 'internal' or code == 'failed' or code == inner) then
    fail(inner, rest) -- a hub code: hub.sql timeout means rolled back (handoff v1, C)
  end
  if not code or LOST[code] then fail('unavailable', text) end -- a Rex code: the outcome is unknown
  fail(code, message)
end
M.rex_error = rex_error

local function hub(action, args)
  local app = S.app
  -- The hub tracks a render only when every hub call of the render reached the hub and returned without
  -- error (hub lead, lead-render-incomplete). So each failure marks the frame first, in a table field:
  -- the mark holds when the plugin catches the error, and the render replies refresh = "all". A render
  -- call other than hub.sql.query reads hub state: it marks the frame too.
  local f = S.frame
  local function failed() if f and f.state then f.state.failed = true end end
  if action ~= 'hub.sql.query' then external() end
  local request = {client_id = app.hub_id, action = action, args = args or {}, timeout_ms = 10000}
  -- No protected call here: hub() runs inside handlers, and in Rex Lua a finished protected call or a
  -- caught error can split the handler's locals from its closures (docs/rex-protocol.md, "Rex Lua
  -- runtime"). rex.json.encode and rex.call return nil and an error; they do not raise (observed on
  -- Rex 0.0.1792158932-g88fd41, lab, 2026-10-22).
  local encoded, encode_err = rex.json.encode(request)
  if encoded == nil then failed(); fail('invalid_input', action .. ': the request cannot be encoded as JSON: ' .. tostring(encode_err)) end
  if #encoded > MAX_REQUEST then failed(); fail('too_large', action .. ': the request has ' .. #encoded .. ' bytes; the limit is ' .. MAX_REQUEST .. '. Nothing was sent.') end
  local response, err = raw.call('client.perform', request)
  if type(response) ~= 'table' then failed(); rex_error(err, action .. ' failed') end
  if type(response.result) ~= 'table' then failed(); fail('internal', action .. ': invalid hub result') end
  return response.result
end

-- Reply parts --------------------------------------------------------------------------------------

-- A reply to the hub over inline_max goes as parts (chunking v1.1, hub lead, with amendments 1-3):
-- hub.reply.part {id, index, count, data} for each slice of the encoded reply, in order, then the
-- stub {lazyrex_parts = {id, count, bytes}} as the reply. The limits come from the hub.register reply.
-- Only the hub resolves a stub, so a reply to any other caller stays whole.
-- PART_REQUEST: the largest encoded hub.reply.part request. The real encoding decides, not a count of
-- escapes (amendment 3): Rex refuses a request over 64 KiB (MAX_REQUEST).
local PART_REQUEST = 60 * 1024

-- reserved answers whether a result has the shape of a stub: an object with the one key
-- lazyrex_parts. A plugin result of that shape is refused, so plugin data never looks like a stub.
local function reserved(result)
  if type(result) ~= 'table' then return false end
  local k = next(result)
  return k == 'lazyrex_parts' and next(result, k) == nil
end
local function check_reserved(result)
  if reserved(result) then fail('reserved_result', 'A reply cannot be an object with the one key lazyrex_parts') end
end

-- boundary answers the largest j' <= j at which s can be cut: s:byte(j' + 1) is not a UTF-8
-- continuation byte.
local function boundary(s, j)
  while j > 0 do
    local b = s:byte(j + 1)
    if not b or b < 0x80 or b > 0xBF then return j end
    j = j - 1
  end
  return j
end

-- slices cuts encoded into strings whose hub.reply.part request is at most PART_REQUEST bytes and
-- whose escaped data is at most part_bytes. A slice that is too large shrinks in proportion and is
-- encoded again (an encode per try, no escape count: Hub Render measured a gsub count as too slow).
local function slices(encoded, id, limits)
  local out, i = {}, 1
  local function request(index, data)
    return rex.json.encode({client_id = S.app.hub_id, action = 'hub.reply.part', args = {id = id, index = index, count = 64, data = data}, timeout_ms = 10000})
  end
  while i <= #encoded do
    local n = limits.part_bytes
    while true do
      local j = boundary(encoded, math.min(#encoded, i + n - 1))
      if j < i then fail('internal', 'The reply cannot be cut into parts') end
      local data = encoded:sub(i, j)
      local size = #request(#out, data)
      local escaped = size - #request(#out, '')
      if size <= PART_REQUEST and escaped <= limits.part_bytes then
        out[#out + 1] = data
        i = j + 1
        break
      end
      n = math.floor((j - i + 1) * math.min(PART_REQUEST / size, limits.part_bytes / escaped) * 0.98)
    end
  end
  if #out > 64 then fail('too_large', 'The reply needs ' .. #out .. ' parts; the limit is 64') end
  return out
end

-- push sends the parts of encoded to the hub and answers the stub. A refused part raises its error.
local function push(encoded, limits)
  S.parts_sent = (S.parts_sent or 0) + 1
  local id = tostring(S.app.instance_id):gsub('[^%w._:-]', '_'):sub(1, 100) .. '-' .. S.parts_sent
  local parts = slices(encoded, id, limits)
  for index, data in ipairs(parts) do hub('hub.reply.part', {id = id, index = index - 1, count = #parts, data = data}) end
  return {lazyrex_parts = {id = id, count = #parts, bytes = #encoded}}
end

-- preflight checks the reply of a handler before its commit, so that a refused reply rolls the
-- transaction back: the reserved shape, and over max_total for the hub. It keeps the encoding for
-- deliver (S.encoded).
local function preflight(result)
  check_reserved(result)
  if not S.from_hub then return end
  local limits = S.app.registration.limits
  local encoded = rex.json.encode(result or {})
  if encoded and #encoded > limits.max_total then fail('too_large', 'The reply is ' .. #encoded .. ' bytes; the limit is ' .. limits.max_total) end
  S.encoded = {value = result, text = encoded}
end

-- parted answers the stub for result when its encoding is over inline_max, after it pushed the parts,
-- or nil when result goes whole. Over max_total it fails with too_large before any part.
local function parted(result, encoded)
  local limits = S.app.registration.limits
  encoded = encoded or rex.json.encode(result)
  if encoded == nil or #encoded <= limits.inline_max then return nil end
  if #encoded > limits.max_total then fail('too_large', 'The reply is ' .. #encoded .. ' bytes; the limit is ' .. limits.max_total) end
  return push(encoded, limits)
end

-- Frames and transactions --------------------------------------------------------------------------

-- A frame is one running handler. tx: true for one transaction (begun at the first SQL call),
-- false for each SQL call alone. render: the render ID of a panel render.
local function frame() return S.frame end

local function sql_call(action, args)
  local f = frame()
  if f and f.tx then
    if not f.token then f.token = hub('hub.sql.begin', {}).tx end
    args.tx = f.token
  end
  if f and f.render then args.render = f.render end
  return hub(action, args)
end

-- run runs fn(...) in a new frame. spec: tx (default true), render, key, fingerprint, store
-- (default true), reply (the frame answers a caller: preflight checks its result before the commit).
-- It commits at the end: with key and store, the reply is saved in the same commit,
-- unless it is a running reply. An error rolls back and raises again.
local function run(spec, fn, ...)
  -- in_render marks a view render. A render never opens a transaction (hub contract); the render token
  -- only names the render for read tracking, and no hub sends it yet.
  local f = {tx = spec.tx ~= false, render = spec.render, in_render = spec.in_render, state = spec.state, notices = {}}
  local outer = S.frame
  S.frame = f
  local ok, result = pcall(fn, ...)
  S.frame = outer
  if not ok then
    if f.token then pcall(hub, 'hub.sql.rollback', {tx = f.token}) end
    error(result, 0)
  end
  local running = type(result) == 'table' and result.status == 'running'
  local save = spec.key ~= nil and spec.store ~= false and not running
  if f.token or save or #f.notices > 0 then
    local out = {result = result} -- a field, not the local: see the note on S at the top
    S.frame = f
    local cok, cerr = pcall(function()
      if spec.reply then preflight(result) end
      if not f.token then f.token = hub('hub.sql.begin', {}).tx end
      local args = {tx = f.token}
      if save then
        -- A large reply to the hub is saved from its parts: the commit names the set, and the reply
        -- is the stub of the same set (one upload, amendment 1).
        args.key, args.fingerprint = spec.key, spec.fingerprint
        f.stub = S.from_hub and parted(result or {}, S.encoded and S.encoded.value == result and S.encoded.text or nil) or nil
        if f.stub then args.reply_parts = f.stub.lazyrex_parts else args.reply = result or {} end
      end
      if #f.notices > 0 then args.notices = f.notices end
      local done = hub('hub.sql.commit', args)
      if done.replayed then out.result = done.reply
      elseif f.stub then S.stub = {result = result, stub = f.stub} end
    end)
    S.frame = outer
    if not cok then
      pcall(hub, 'hub.sql.rollback', {tx = f.token})
      error(cerr, 0)
    end
    return out.result
  end
  return result
end

-- with_replay runs fn in a frame, after it checks the saved reply of key, then of each key in
-- spec.legacy_keys (the same key of the caller's legacy IDs). A call without a key, or with
-- store = false, always runs fn. A new reply is saved under key only.
local function with_replay(spec, fn, ...)
  spec.reply = true
  if spec.key ~= nil and spec.store ~= false then
    local saved = hub('hub.sql.replay', {key = spec.key, fingerprint = spec.fingerprint})
    if saved.found then return saved.reply end
    for _, k in ipairs(spec.legacy_keys or {}) do
      saved = hub('hub.sql.replay', {key = k, fingerprint = spec.fingerprint})
      if saved.found then return saved.reply end
    end
  end
  return run(spec, fn, ...)
end

-- Database ---------------------------------------------------------------------------------------

local db = {}
-- no_write refuses a write in a view render: a render reads the committed state only.
local function no_write(name)
  local f = frame()
  if f and f.in_render then fail('refused', 'A render cannot write: ' .. name .. ' runs in an action or a select handler') end
end
-- db.exec(sql, ...) runs one statement. It answers {changes, id}, and rows for RETURNING.
function db.exec(sql, ...)
  no_write('db.exec')
  if type(sql) ~= 'string' then fail('invalid_input', 'db.exec needs SQL text') end
  local r = sql_call('hub.sql.exec', {sql = sql, params = params(select('#', ...), ...)})
  local out = {changes = r.changes, id = decode_value(r.id)}
  if r.columns then out.rows = rows_of(r) end
  return out
end
-- db.query(sql, ...) answers every row, as a list of rows keyed by column name. A result that does
-- not fit in one reply fails with too_large: a partial result is never answered (docs/sql.md
-- section 4, paging). Use db.each, or LIMIT, for a big result.
function db.query(sql, ...)
  if type(sql) ~= 'string' then fail('invalid_input', 'db.query needs SQL text') end
  local r = sql_call('hub.sql.query', {sql = sql, params = params(select('#', ...), ...)})
  if r.complete == false then
    fail('too_large', 'The result of db.query does not fit in one reply (' .. #(r.rows or {}) .. ' rows came back). Use app.db.each, or LIMIT.')
  end
  return rows_of(r)
end
-- db.first(sql, ...) answers the first row, or nil. It reads one reply only.
function db.first(sql, ...)
  if type(sql) ~= 'string' then fail('invalid_input', 'db.first needs SQL text') end
  return rows_of(sql_call('hub.sql.query', {sql = sql, params = params(select('#', ...), ...)}))[1]
end
-- db.each(sql, ..., fn) calls fn(row) for every row of a result of any size, and answers the row
-- count. It reads all pages first, in one transaction (one snapshot), then calls fn: a write in fn
-- cannot move the pages. The next page is the same query with OFFSET = the rows read so far: each
-- page runs the query again, so a big result costs pages x rows. The newline before ")" keeps a
-- line comment at the end of sql from commenting it out; trailing ";" and blanks are removed. Give
-- ORDER BY for a defined order: the order of a subquery is SQLite behavior, not a guarantee.
function db.each(sql, ...)
  local n = select('#', ...)
  local fn = n > 0 and select(n, ...) or nil
  if type(sql) ~= 'string' then fail('invalid_input', 'db.each needs SQL text') end
  if type(fn) ~= 'function' then fail('invalid_input', 'db.each needs a function as its last argument') end
  local list = params(n - 1, ...)
  -- A render reads with no transaction; there the pages are not one snapshot.
  local f = frame()
  local within = (f and f.in_render) and function(g) return g() end or db.transaction
  local rows = within(function()
    local r = sql_call('hub.sql.query', {sql = sql, params = list})
    local out = rows_of(r)
    local base = (sql:gsub('[%s;]+$', ''))
    while r.complete == false do
      if #(r.rows or {}) == 0 then fail('too_large', 'db.each: a reply came back with no rows') end
      local p = {}
      for i, v in ipairs(list) do p[i] = v end
      p[#list + 1] = #out
      r = sql_call('hub.sql.query', {sql = 'SELECT * FROM (' .. base .. '\n) LIMIT -1 OFFSET ?' .. (#list + 1), params = p})
      for _, row in ipairs(rows_of(r)) do out[#out + 1] = row end
    end
    return out
  end)
  for _, row in ipairs(rows) do fn(row) end
  return #rows
end
-- db.batch(statements) runs {sql, ...values} statements in one transaction, all or nothing. In a
-- handler transaction it runs inside it.
function db.batch(statements)
  no_write('db.batch')
  local list = {}
  for i, s in ipairs(statements) do
    list[i] = {sql = s[1], params = params(math.max(#s - 1, s.n and s.n - 1 or 0), unpack(s, 2, s.n or #s))}
  end
  local r = sql_call('hub.sql.batch', {statements = list})
  local out = {}
  for i, x in ipairs(r.results or {}) do out[i] = {changes = x.changes, id = decode_value(x.id), rows = x.columns and rows_of(x) or nil} end
  return out
end
-- db.transaction(fn) runs fn in one transaction: for {tx = false} handlers, timers with
-- {tx = false} and start code. Inside a handler transaction it only calls fn.
function db.transaction(fn, ...)
  no_write('db.transaction')
  local f = frame()
  if f and f.tx then return fn(...) end
  return run({tx = true}, fn, ...)
end
-- db.int and db.blob tag exact values (as lazyrex.int and lazyrex.blob).
db.int, db.blob = M.int, M.blob

-- Schemas ----------------------------------------------------------------------------------------

local function copy(v)
  if type(v) ~= 'table' then return v end
  local r = {}
  for k, x in pairs(v) do r[k] = copy(x) end
  return r
end
local KEY = {type = 'string', minLength = 1, maxLength = 200, description = 'Choose a key, and retry with the same content after an unknown outcome.'}
-- schema_of turns the short form {text = 'string', note = 'string?'} into a JSON Schema. A table
-- with a type field is already a JSON Schema. A short-form value can also be a JSON Schema (with
-- optional = true for an optional property).
local function schema_of(s)
  local out
  if type(s) == 'table' and s.type ~= nil then
    out = copy(s)
  else
    local props, required = {}, {}
    for name, v in pairs(s or {}) do
      local optional = false
      if type(v) == 'string' then
        optional = v:sub(-1) == '?'
        props[name] = {type = optional and v:sub(1, -2) or v}
      elseif type(v) == 'table' then
        props[name] = copy(v)
        optional = v.optional == true
        props[name].optional = nil
      else
        error('schema: property ' .. tostring(name) .. ' needs a type name or a JSON Schema', 3)
      end
      if not optional then required[#required + 1] = name end
    end
    table.sort(required)
    out = {type = 'object', properties = props, additionalProperties = false}
    if #required > 0 then out.required = required end
  end
  if out.type == 'object' then
    out.properties = out.properties or {}
    if out.properties.key == nil then out.properties.key = copy(KEY) end
  end
  return out
end
-- chars answers the number of characters of UTF-8 text s: the bytes that are not continuation
-- bytes (0x80-0xBF). JSON Schema minLength and maxLength count characters, as the hub does (runes).
local function chars(s) return #(s:gsub('[\128-\191]', '')) end
local function json_type(v)
  local t = type(v)
  if t == 'number' then return v % 1 == 0 and 'integer' or 'number' end
  if t == 'table' then return next(v) ~= nil and v[1] ~= nil and 'array' or 'object' end
  return t
end
-- check checks args against the parts of a JSON Schema that the SDK knows: type (a name or a list), required,
-- additionalProperties = false, and the type, minLength, maxLength, minimum, maximum and enum of
-- each property. The hub can check more.
local function check(schema, args)
  if type(args) ~= 'table' then fail('invalid_input', 'The arguments must be an object') end
  local props = schema.properties or {}
  for _, name in ipairs(schema.required or {}) do
    if args[name] == nil then fail('invalid_input', 'Missing argument ' .. name) end
  end
  for name, v in pairs(args) do
    local p = props[name]
    if p == nil then
      if schema.additionalProperties == false then fail('invalid_input', 'Unknown argument ' .. tostring(name)) end
    else
      -- type is one name or a list of names (JSON Schema). An empty table is an object or an empty
      -- array: Lua cannot tell them apart. An unknown type name is not checked.
      local have = json_type(v)
      local wants = type(p.type) == 'table' and p.type or {p.type}
      local fits = p.type == nil
      for _, want in ipairs(wants) do
        if want == have or (want == 'number' and have == 'integer') or (want == 'array' and have == 'object' and next(v) == nil)
          or not (want == 'string' or want == 'integer' or want == 'number' or want == 'boolean' or want == 'object' or want == 'array') then fits = true end
      end
      if not fits then fail('invalid_input', 'Argument ' .. name .. ' must be ' .. table.concat(wants, ' or ')) end
      if type(v) == 'string' and p.minLength and chars(v) < p.minLength then fail('invalid_input', 'Argument ' .. name .. ' is too short') end
      if type(v) == 'string' and p.maxLength and chars(v) > p.maxLength then fail('invalid_input', 'Argument ' .. name .. ' is too long') end
      if type(v) == 'number' and p.minimum and v < p.minimum then fail('invalid_input', 'Argument ' .. name .. ' is too small') end
      if type(v) == 'number' and p.maximum and v > p.maximum then fail('invalid_input', 'Argument ' .. name .. ' is too large') end
      if p.enum then
        local found = false
        for _, e in ipairs(p.enum) do if e == v then found = true end end
        if not found then fail('invalid_input', 'Argument ' .. name .. ' has an unknown value') end
      end
    end
  end
end

-- The plugin ---------------------------------------------------------------------------------------

-- Routes with a query. A form route can carry values (form v1 has no hidden field), for example
-- /forms/release?profile=p&attempt=a. M.route(path, query) builds one; ctx.query has the decoded
-- values (a malformed escape drops that pair).
local function encode_part(s) return (tostring(s):gsub('[^%w%-%._~]', function(c) return string.format('%%%02X', c:byte()) end)) end
function M.route(path, query)
  local keys = {}
  for k in pairs(query or {}) do keys[#keys + 1] = k end
  if #keys == 0 then return path end
  table.sort(keys)
  local parts = {}
  for _, k in ipairs(keys) do parts[#parts + 1] = encode_part(k) .. '=' .. encode_part(query[k]) end
  return path .. '?' .. table.concat(parts, '&')
end
local function decode_part(s)
  if s:find('%%[^%x]') or s:find('%%%x?$') or s:find('%%%x[^%x]') then return nil end
  return (s:gsub('+', ' '):gsub('%%(%x%x)', function(h) return string.char(tonumber(h, 16)) end))
end
local function query_of(route)
  local out = {}
  local q = type(route) == 'string' and route:match('%?(.*)$')
  for pair in (q or ''):gmatch('[^&]+') do
    local k, v = pair:match('^([^=]*)=(.*)$')
    k, v = decode_part(k or pair), decode_part(v or '')
    if k and v and k ~= '' then out[k] = v end
  end
  return out
end

-- slug turns a panel title into a command id: the hub accepts only a-z, 0-9 and _ (observed on
-- hub/sql-r2 c840c92: 'command id "x.y": use a-z, 0-9 and _').
local function slug(text) return (tostring(text):lower():gsub('[^%w]+', '_'):gsub('^_+', ''):gsub('_+$', '')) end
-- check_command checks the command id at the declaration, so a bad id fails with a clear message.
local function check_command(spec)
  assert(type(spec.id) == 'string' and spec.id:match('^[a-z0-9_]+$'), 'A command id uses only a-z, 0-9 and _ (not ' .. tostring(spec.id) .. ')')
  return spec
end
local function part(s) s = tostring(s); return #s .. ':' .. s end

-- json_decode is app.json.decode(text): the value of JSON text, nil for JSON null. rex.json.decode
-- does not raise: invalid JSON gives nil and an error text, and JSON null gives nil alone (hub lead
-- lab, Rex 0.0.1792158932-g88fd41). So it checks the error text, not the value, and fails with
-- internal: the plugin wrote the text. The message quotes at most 80 bytes, cut at a UTF-8 character
-- boundary, so the error reply stays valid UTF-8.
local function json_decode(text)
  if type(text) ~= 'string' then fail('internal', 'invalid JSON: app.json.decode takes text, not a ' .. type(text)) end
  local value, err = rex.json.decode(text)
  if err ~= nil then fail('internal', 'invalid JSON: ' .. tostring(err) .. '; text starts with ' .. text:sub(1, boundary(text, math.min(#text, 80)))) end
  return value
end

-- M.plugin(name) answers the app. Call it once, in the main file. The SDK registers the plugin
-- after the main file has run (rex.after 0): first the migrations, then the start functions, then
-- client.keymap and hub.register.
function M.plugin(name)
  assert(S.app == nil, 'lazyrex.plugin is called once')
  assert(type(name) == 'string' and name:match('^[%l%d_%-]+$'), 'A plugin name is a-z, 0-9, _ and -')
  local args = rex.args or {}
  local app = {name = name, hub_id = assert(args.hub_client_id, 'hub_client_id is required (the hub starts the plugin)'),
    instance_id = assert(args.instance_id, 'instance_id is required'), session_id = args.plugin_session or rex.session_id,
    args = {}, db = db, ui = {}, fail = fail, int = M.int, blob = M.blob, route = M.route,
    json = {decode = json_decode}}
  for k, v in pairs(args) do
    if k ~= 'hub_client_id' and k ~= 'instance_id' and k ~= 'plugin_session' and k ~= 'lazyrex_sdk' and k ~= 'main' then app.args[k] = v end
  end
  app.dir = args.main and args.main:match('^(.*)/[^/]*$')
  S.app = app
  local actions, order, panels, views, commands, starts, migrations = {}, {}, {}, {}, {}, {}, {}
  local migrate_base, migrate_called = 1, false -- the schema version of migrations[1]; a call came
  local listeners = {}

  -- app.hub(action, args) calls the hub. A hub error raises {code, message}: a refusal stays final.
  -- A raw hub.sql call in a render carries the render ID too: the hub tracks no reads for a render
  -- that also reads without it (hub lead, 2026-10-22). A copy keeps the caller's table as it was.
  function app.hub(action, a)
    local f = frame()
    if f and f.render and type(action) == 'string' and action:sub(1, 8) == 'hub.sql.' and type(a) == 'table' and a.render == nil then
      a = copy(a)
      a.render = f.render
    end
    return hub(action, a)
  end
  -- app.rex(method, args) calls Rex. A Rex error raises {code, message}.
  function app.rex(method, a)
    local r, err = rex.call(method, a or {})
    if r == nil then rex_error(err, method .. ' failed') end
    return r
  end
  -- app.refresh() renders every open view of the plugin again (state outside SQL).
  -- app.refresh ignores a failure. It raises and catches nothing: in Rex Lua a caught error splits the
  -- locals of the running handler from its closures (docs/rex-protocol.md, "Rex Lua runtime").
  function app.refresh()
    raw.call('client.perform', {client_id = app.hub_id, action = 'hub.ui.invalidate', args = {}, timeout_ms = 10000})
  end
  -- app.notice(agent, pending, opts) wakes agent at the commit of the current handler. opts.once = true
  -- (notice v2, hub/nudge-once): a new nudge only if the agent is not pending or read since the last
  -- one; it needs pending = true. app.notice_read(agent) records that the agent read (pending stays).
  -- A hub without notice v2 ignores once and refuses read.
  local function queue(name, notice)
    local f = frame()
    if not f then fail('invalid_input', name .. ' runs in a handler') end
    no_write(name)
    f.notices[#f.notices + 1] = notice
  end
  function app.notice(agent, pending, opts)
    local n = {agent = agent, pending = pending == true}
    if opts and opts.once then
      if not n.pending then fail('invalid_input', 'app.notice: once needs pending = true') end
      n.once = true
    end
    queue('app.notice', n)
  end
  function app.notice_read(agent) queue('app.notice_read', {agent = agent, read = true}) end

  -- app.db.migrate {sql, ...}: each entry is one schema version (a SQL text or a function). The
  -- SDK runs the new ones in order, in one transaction, and sets PRAGMA user_version.
  -- {base = N, sql, ...}: entry 1 makes schema version N in one step, and entry i is version
  -- N + i - 1. Only a new database runs the base (docs/plugin-sdk.md, "Migrations").
  -- PRAGMA user_version is a 32-bit signed integer in SQLite, so base and the last version are at most
  -- MAX_VERSION. The range check also refuses NaN and infinity.
  local MAX_VERSION = 2147483647
  function db.migrate(list)
    if list.base ~= nil then
      local b = list.base
      if type(b) ~= 'number' or not (b >= 1 and b <= MAX_VERSION) or b ~= math.floor(b) then
        error('app.db.migrate: base must be an integer from 1 to ' .. MAX_VERSION, 2)
      end
      if migrate_called then error('app.db.migrate: give base in the first call', 2) end
      migrate_base = b
    end
    migrate_called = true
    for _, step in ipairs(list) do migrations[#migrations + 1] = step end
    if migrate_base + #migrations - 1 > MAX_VERSION then
      error('app.db.migrate: the last schema version must be at most ' .. MAX_VERSION, 2)
    end
  end

  -- app.action(name, schema, fn, opts): an action for agents (and the user). opts: user_only,
  -- tx = false, store = false, title, description, fingerprint = function(args) -> text.
  function app.action(action_name, schema, fn, opts)
    opts = opts or {}
    assert(type(action_name) == 'string' and action_name ~= '', 'An action needs a name')
    assert(type(fn) == 'function', 'An action needs a function')
    assert(not actions[action_name], 'The action ' .. action_name .. ' exists')
    actions[action_name] = {name = action_name, schema = schema_of(schema), fn = fn, opts = opts}
    order[#order + 1] = action_name
  end

  -- opts.refresh (app.panel and app.view): absent (the hub renders the view again after a write to a
  -- table that its render read) or "all" (after each SQL commit of the plugin).
  local function refresh_of(opts)
    local v = opts.refresh
    assert(v == nil or v == 'all', 'refresh is "all" or absent')
    return v
  end

  -- app.panel(title, fn, opts): a palette command (surface panel) that the hub opens in its side slot
  -- (a split, or a floating layer); it makes no tab. fn(view) answers a list of nodes
  -- (app.ui.*). opts: id (default from the title), description, keywords, landing (default true),
  -- refresh.
  function app.panel(title, fn, opts)
    opts = opts or {}
    local id = opts.id or slug(title)
    check_command({id = id})
    local route = '/panels/' .. id
    panels[route] = {title = title, fn = fn, refresh = refresh_of(opts)}
    commands[#commands + 1] = {id = id, title = title, description = opts.description or ('Show ' .. title), keywords = opts.keywords or {id},
      route = route, surface = 'panel', landing = opts.landing ~= false, user_only = 'navigation'}
  end

  -- app.view(route, handlers, opts): a raw view. A route that ends with '*' matches each route with
  -- that prefix. handlers.render(ctx, args) answers the whole tree. handlers[<hub view action>]
  -- (view.action, view.draft.save, view.send, view.seen, ...) get (ctx, args) in one transaction
  -- with ctx.key = the operation key. opts.store = false saves no reply; opts.tx = false runs each SQL
  -- call alone (long operations: use app.db.transaction for the steps).
  function app.view(route, handlers, opts)
    opts = opts or {}
    views[#views + 1] = {route = route, prefix = route:sub(-1) == '*' and route:sub(1, -2) or nil, handlers = handlers, opts = opts, refresh = refresh_of(opts)}
  end
  -- app.command(spec): an extra hub.register command (a palette entry).
  function app.command(spec) commands[#commands + 1] = check_command(spec) end

  -- app.start(fn, opts): runs once at start, after the registration and the migrations, in one
  -- transaction. opts.after_register = true runs it after the other start functions.
  function app.start(fn, opts) starts[#starts + 1] = {fn = fn, opts = opts or {}} end

  -- Callbacks: each run is a frame with one transaction (opts.tx = false: each SQL call alone). An
  -- error is printed to the plugin log; it does not stop the plugin.
  local function callback(label, fn, opts, ...)
    local ok, err = pcall(run, {tx = (opts or {}).tx ~= false}, fn, ...)
    if not ok then print('lazyrex: ' .. label .. ' failed: ' .. normalize(err).message) end
  end
  -- app.on(event, fn): a Rex event (fn(ev, ctx): ctx is the Rex event context, with block_id and
  -- session_id) or the hub calls hub.changed, hub.prompt and view.seen (fn(args)). An error in a hub
  -- call listener reaches the hub as the error reply (the hub then calls again).
  function app.on(event, fn, opts)
    if event == 'hub.changed' or event == 'hub.prompt' or event == 'view.seen' then
      listeners[event] = listeners[event] or {}
      table.insert(listeners[event], {fn = fn, opts = opts or {}})
      return
    end
    rex.on(event, function(ctx, ev) callback(event, fn, opts, ev, ctx) end)
  end
  function app.after(seconds, fn, opts) rex.after(seconds, function() callback('timer', fn, opts) end) end
  function app.every(seconds, fn, opts)
    local function tick() callback('timer', fn, opts); rex.after(seconds, tick) end
    rex.after(seconds, tick)
  end

  -- UI nodes. An item or widget with on_select gets an action. An agent action that does the same
  -- (agent parity) goes in tool; without one, user_only gives the reason (default 'navigation').
  local function action_of(n)
    if not n.on_select then return n.action end
    local a = {name = 'select', label = n.action_label or 'Select', enabled = n.enabled ~= false}
    if n.tool then a.tool = n.tool else a.user_only = n.user_only or 'navigation' end
    return a
  end
  function app.ui.list(id, items, opts)
    opts = opts or {}
    return {id = id, type = 'list', label = opts.label, items = items, empty = opts.empty}
  end
  function app.ui.widget(id, text, on_select, opts)
    opts = opts or {}
    return {id = id, type = 'widget', text = text, on_select = on_select, tool = opts.tool, user_only = opts.user_only,
      action_label = opts.label, enabled = opts.enabled}
  end
  function app.ui.text(id, text) return {id = id, type = 'widget', text = text} end
  function app.ui.error(id, text) return {id = id, type = 'error', text = text} end

  -- wire_nodes answers the wire form of app.ui nodes: functions are dropped, actions added. A node
  -- with no type is a widget. An empty list becomes a widget: Rex Lua cannot send an empty JSON array.
  local function wire_nodes(nodes)
    local out = {}
    for i, n in ipairs(nodes or {}) do
      local node = {id = tostring(n.id), type = n.type or 'widget', label = n.label, text = n.text, action = action_of(n)}
      if n.type == 'list' then
        if #(n.items or {}) == 0 then
          node = {id = tostring(n.id), type = 'widget', label = n.label, text = n.empty or 'Nothing here yet.'}
        else
          node.items = {}
          for j, item in ipairs(n.items) do
            node.items[j] = {id = tostring(item.id), label = tostring(item.label or item.id), detail = item.detail or '', action = action_of(item)}
          end
        end
      end
      out[i] = node
    end
    return out
  end
  -- tree answers the view tree of a panel.
  local function tree(panel, nodes)
    local out = wire_nodes(nodes)
    if #out == 0 then out[1] = {id = 'empty', type = 'widget', text = 'Nothing here yet.'} end
    return {title = panel.title, nodes = out}
  end
  local function find_node(nodes, node_id, item_id)
    for _, n in ipairs(nodes or {}) do
      if tostring(n.id) == node_id then
        if n.type ~= 'list' then return n end
        for _, item in ipairs(n.items or {}) do if tostring(item.id) == item_id then return item end end
        return nil
      end
    end
  end
  local function view_of(route)
    for _, v in ipairs(views) do
      -- A route matches itself, itself with a query ('?a=b'), or a prefix route ('...*').
      local r = tostring(route)
      if v.route == r or r:sub(1, #v.route + 1) == v.route .. '?' or (v.prefix and r:sub(1, #v.prefix) == v.prefix) then return v end
    end
  end

  -- app.form(route, spec): a form view (form v1: text, multiline, boolean and select fields). spec:
  --   title: text, or fn(ctx) -> text.
  --   fields: a list, or fn(ctx) -> list of {id, label, kind, value, required, options, ...}. The
  --     function runs at each render and again at the submit, in its transaction. A select field
  --     with check = false takes any text (for a value that the render fixed, such as a set key).
  --   intro: text, or fn(ctx) -> text: a widget above the form; or a list of nodes ({id, text}
  --     widgets, app.ui.widget and app.ui.list with on_select).
  --   after: a list of nodes, or fn(ctx) -> list: nodes below the form, above Cancel.
  --   instead: fn(ctx) -> nil, or {nodes, status}: another state of the route. Those nodes replace
  --     the form (intro and after stay), and a submit fails with conflict.
  --   id, label: the form node ID (default 'form') and its label.
  --   submit: {label, tool = <agent action> | user_only = <reason>, enabled = bool | fn(ctx),
  --     refusal = text | fn(ctx): the conflict message of a submit when enabled is false}.
  --   on_submit(ctx, values) -> result. nil closes the form; a table passes through ({form},
  --     {navigate}, {close_form}, {status = 'running'}, ...).
  --   cancel: the Cancel label (default 'Cancel'); false: no Cancel.
  --   command: a palette command that opens the form ({id, title, description, keywords}).
  --   tx: as for app.action. No store = false: the value checks run before a private replay.
  -- An app.fail in title, fields, intro, after or instead shows the message in place of the form.
  -- The SDK checks the values first: a required field must be filled in (a required boolean must be
  -- true), a select value must be an option, and an unknown field is refused. An on_select of a node
  -- in intro, after or instead runs in the transaction of the click, after the SDK evaluates the
  -- nodes again (as for a panel).
  function app.form(route, spec)
    local form_id = spec.id or 'form'
    local function eval(v, ctx) if type(v) == 'function' then return v(ctx) end return v end
    local function intro_of(ctx)
      local intro = eval(spec.intro, ctx)
      if type(intro) == 'table' then return intro end
      if intro ~= nil and intro ~= '' then return {{id = 'intro', type = 'widget', text = tostring(intro)}} end
      return {}
    end
    -- state answers the parts of the route for ctx: {intro, after, instead}.
    local function state(ctx)
      return {intro = intro_of(ctx), after = eval(spec.after, ctx) or {}, instead = spec.instead and spec.instead(ctx) or nil}
    end
    local function render_form(ctx)
      -- out fields, not locals: a closure must not reassign a captured local (see S at the top).
      local out = {title = '', nodes = {}, status = {state = 'blocked', kind = 'question', msg = 'Waiting for input'}}
      local ok, err = pcall(function()
        out.title = tostring(eval(spec.title, ctx) or '')
        local st = state(ctx)
        local nodes = {}
        for _, n in ipairs(st.intro) do nodes[#nodes + 1] = n end
        if st.instead then
          for _, n in ipairs(st.instead.nodes or {}) do nodes[#nodes + 1] = n end
          if st.instead.status then out.status = st.instead.status end
        else
          local submit = spec.submit or {}
          local action = {name = submit.name or 'submit', label = submit.label or 'Submit', enabled = eval(submit.enabled, ctx) ~= false}
          if submit.tool then action.tool = submit.tool else action.user_only = submit.user_only or 'navigation' end
          nodes[#nodes + 1] = {id = form_id, type = 'form', label = spec.label, fields = eval(spec.fields, ctx) or {}, submit_action = action}
        end
        for _, n in ipairs(st.after) do nodes[#nodes + 1] = n end
        out.nodes = wire_nodes(nodes)
        for i, n in ipairs(nodes) do
          if n.type == 'form' then out.nodes[i].fields, out.nodes[i].submit_action = n.fields, n.submit_action end
        end
      end)
      local nodes = ok and out.nodes or {{id = 'error', type = 'error', text = normalize(err).message}}
      if spec.cancel ~= false then
        nodes[#nodes + 1] = {id = 'cancel', type = 'widget', text = '', action = {name = '$cancel', label = spec.cancel or 'Cancel', enabled = true}}
      end
      return {title = out.title, nodes = nodes, status = out.status}
    end
    local function checked(fields, values)
      local known, out = {}, {}
      for _, f in ipairs(fields or {}) do
        known[f.id] = true
        local v = values[f.id]
        local label = f.label or f.id
        local empty = v == nil or v == '' or (f.kind == 'boolean' and v ~= true)
        if f.required and empty then fail('invalid_input', (f.kind == 'boolean' and 'Check ' or 'Fill in ') .. label) end
        if v ~= nil then
          local want = f.kind == 'boolean' and 'boolean' or 'string'
          if type(v) ~= want then fail('invalid_input', label .. ' must be ' .. want) end
          if f.kind == 'select' and v ~= '' and f.check ~= false then
            local found = false
            for _, o in ipairs(f.options or {}) do if o.value == v then found = true end end
            if not found then fail('invalid_input', label .. ': choose one of the options') end
          end
          out[f.id] = v
        end
      end
      for id in pairs(values) do if not known[id] then fail('invalid_input', 'Unknown field ' .. tostring(id)) end end
      return out
    end
    app.view(route, {
      render = function(ctx) return render_form(ctx) end,
      ['view.action'] = function(ctx, a)
        local submit = spec.submit or {}
        local st = state(ctx)
        if a.node_id == form_id and a.action == (submit.name or 'submit') then
          local stale = 'This form cannot be sent now. It shows the current state.'
          if st.instead then fail('conflict', stale) end
          if eval(submit.enabled, ctx) == false then fail('conflict', tostring(eval(submit.refusal, ctx) or stale)) end
          local values = type(ctx.input.values) == 'table' and ctx.input.values or {}
          local result = spec.on_submit(ctx, checked(eval(spec.fields, ctx), values))
          if result == nil then return {close_form = true} end
          return result
        end
        local nodes = {}
        for _, part_nodes in ipairs({st.intro, st.instead and st.instead.nodes or {}, st.after}) do
          for _, n in ipairs(part_nodes) do nodes[#nodes + 1] = n end
        end
        local n = find_node(nodes, tostring(a.node_id), ctx.input.item_id ~= nil and tostring(ctx.input.item_id) or nil)
        if not n or not n.on_select then fail('not_found', 'This action is gone. The form shows the current state.') end
        if n.enabled == false then fail('conflict', 'This action is not available now. The form shows the current state.') end
        return n.on_select(ctx) or {}
      end,
    }, {tx = spec.tx, store = spec.store})
    if spec.command then
      local c = copy(spec.command)
      c.route = c.route or route
      -- Agent parity, as the submit: the tool, else the reason. A command that names action or user_only
      -- keeps it (the hub refuses a command with both).
      if c.action == nil and c.user_only == nil then
        local submit = spec.submit or {}
        if submit.tool then c.action = submit.tool else c.user_only = submit.user_only or 'navigation' end
      end
      commands[#commands + 1] = check_command(c)
    end
  end

  -- Callers ----------------------------------------------------------------------------------------
  local function identify(pid)
    local r = hub('hub.identify', {pid = pid})
    if r.found ~= true or type(r.agent) ~= 'string' or r.agent == '' then fail('refused', 'The hub does not recognize this agent') end
    return {agent = r.agent, session_id = r.session_id, block_id = r.block_id, requester_pid = pid, aliases = r.legacy or {}, identity = r}
  end
  -- The saved-reply key of a caller: the user and each agent have their own key space.
  local function op_key(ctx, key)
    if key == nil then return nil end
    if ctx.agent then return 'a/' .. part(ctx.agent) .. part(key) end
    return 'u/' .. part(key)
  end

  -- call_action checks the arguments and runs action a. An agent gives its key as an argument. The
  -- user path keeps the UI operation key (ctx.key from user_ctx); an argument key is refused there.
  local function call_action(a, ctx, call_args)
    check(a.schema, call_args)
    if ctx.user then
      if call_args.key ~= nil then fail('invalid_input', 'The user path takes the operation key, not a key argument') end
    else
      ctx.key = call_args.key
    end
    local inner = {}
    for k, v in pairs(call_args) do if k ~= 'key' then inner[k] = v end end
    local fp = a.opts.fingerprint and a.opts.fingerprint(inner) or M.fingerprint({action = a.name, args = inner})
    local spec = {tx = a.opts.tx ~= false, store = a.opts.store, key = op_key(ctx, ctx.key), fingerprint = fp, legacy_keys = {}}
    if ctx.key ~= nil then
      for _, id in ipairs(ctx.aliases or {}) do spec.legacy_keys[#spec.legacy_keys + 1] = op_key({agent = id}, ctx.key) end
    end
    return with_replay(spec, a.fn, ctx, inner)
  end
  local function exported(n) local a = actions[n]; return a and not a.opts.user_only end

  local function user_ctx(a)
    local context = type(a.context) == 'table' and a.context or {}
    -- source: the source pane directory that the hub froze at the open (hub/form-source-pwd d407808):
    -- pwd is the raw terminal.pwd value (a file:// URI); pwd_error the reason when the read gave none.
    return {user = true, key = a.operation_key, route = a.route, input = type(a.input) == 'table' and a.input or {}, args = a,
      session_id = context.session_id, view_id = a.view_id, context = context, query = query_of(a.route),
      source = {pwd = context.source_pwd, pwd_error = context.source_pwd_error}}
  end
  -- sized answers the render reply, or fails with too_large over max_total (the status included). It
  -- keeps the encoding for the reply exit, which sends a large reply in parts.
  local function sized(reply)
    local encoded = rex.json.encode(reply)
    local max = app.registration.limits.max_total
    if #encoded > max then fail('too_large', 'The view has ' .. #encoded .. ' bytes; the limit is ' .. max .. '. Show fewer items.') end
    S.encoded = {value = reply, text = encoded}
    return reply
  end
  -- render_ctx answers the context of a render. ctx.refresh_all() replies refresh = "all" for this
  -- render only (a view that serves many routes). It writes a field of the render frame.
  local function render_ctx(a)
    local ctx = user_ctx(a)
    ctx.refresh_all = function()
      local f = S.frame
      if f and f.state then f.state.all = true end
    end
    return ctx
  end
  -- refreshed answers the refresh field of a render reply: "all" when the view or the render asked for
  -- it, a hub call failed, or the render read outside SQL; otherwise none (tracked).
  local function refreshed(state, decl)
    if decl or state.all or state.failed or state.external or S.untracked then return 'all' end
    return nil
  end
  local function view_call(action, a)
    local panel = panels[a.route]
    if action == 'view.render' then
      if panel then
        local state = {}
        local t = run({tx = false, render = a.render, in_render = true, state = state}, function() return tree(panel, panel.fn(render_ctx(a))) end)
        return sized({tree = t, refresh = refreshed(state, panel.refresh)})
      end
      local v = view_of(a.route)
      if v and v.handlers.render then
        local state = {}
        local t = run({tx = false, render = a.render, in_render = true, state = state}, v.handlers.render, render_ctx(a), a)
        return sized({tree = t, refresh = refreshed(state, v.refresh)})
      end
      fail('not_found', 'Unknown route ' .. tostring(a.route))
    end
    local ctx = user_ctx(a)
    local key = op_key(ctx, a.operation_key)
    -- As the hub's view_ops fingerprint (viewsvc.go viewAdmit): the view, its route and its context
    -- are part of it, so the same key from another view is an operation conflict.
    local fp = M.fingerprint({action = action, view_id = a.view_id, context = a.context, route = a.route, node_id = a.node_id, name = a.action, input = a.input, text = a.text,
      base_revision = a.base_revision, draft_revision = a.draft_revision})
    if panel and action == 'view.action' then
      -- The panel runs again in the click's transaction, so the handler sees the current row.
      return with_replay({tx = true, key = key, fingerprint = fp}, function()
        local nodes = panel.fn(ctx)
        local n = find_node(nodes, tostring(a.node_id), ctx.input.item_id ~= nil and tostring(ctx.input.item_id) or nil)
        if not n or not n.on_select then fail('not_found', 'This item is gone. The panel shows the current items.') end
        if n.enabled == false then fail('conflict', 'This action is not available now. The panel shows the current state.') end
        return n.on_select(ctx) or {}
      end)
    end
    local v = view_of(a.route)
    local h = v and v.handlers[action]
    if h then return with_replay({tx = v.opts.tx ~= false, key = key, fingerprint = fp, store = v.opts.store}, h, ctx, a) end
    if action == 'view.seen' then return {} end
    fail('invalid_input', 'Unknown action ' .. tostring(action) .. ' on ' .. tostring(a.route))
  end

  local function dispatch(ev)
    local from_hub = ev.requester and ev.requester.client_id == app.hub_id
    local a = type(ev.args) == 'table' and ev.args or {}
    if ev.action == 'hub.forward' then
      if not from_hub then fail('refused', 'Only the hub forwards agent actions') end
      if not exported(a.action) then fail('refused', 'The forwarded action is not exported') end
      if type(a.requester_pid) ~= 'number' or a.requester_pid <= 0 or a.requester_pid % 1 ~= 0 then fail('invalid_input', 'A positive requester PID is required') end
      return call_action(actions[a.action], identify(a.requester_pid), type(a.args) == 'table' and a.args or {})
    end
    if exported(ev.action) then
      local p = ev.requester and ev.requester.principal
      if type(p) ~= 'table' or p.kind ~= 'peer' or type(p.pid) ~= 'number' then fail('refused', 'A local requester PID is required') end
      return call_action(actions[ev.action], identify(p.pid), a)
    end
    if not from_hub then fail('refused', 'Only the hub calls ' .. tostring(ev.action)) end
    if ev.action == 'hub.changed' or (ev.action == 'hub.prompt' and listeners['hub.prompt']) then
      for _, l in ipairs(listeners[ev.action] or {}) do run({tx = l.opts.tx ~= false}, l.fn, a) end
      return {}
    end
    if a.user ~= 'user' then fail('refused', 'Unknown user') end
    if actions[ev.action] then return call_action(actions[ev.action], user_ctx(a), a.args or {}) end -- user_only
    if ev.action == 'view.seen' then
      for _, l in ipairs(listeners['view.seen'] or {}) do run({tx = l.opts.tx ~= false}, l.fn, a) end
      local v = view_of(a.route)
      if v and v.handlers['view.seen'] then return view_call(ev.action, a) end
      return {}
    end
    if ev.action:sub(1, 5) == 'view.' then return view_call(ev.action, a) end
    fail('not_supported', 'Unknown action ' .. tostring(ev.action))
  end

  -- deliver answers the reply for result, after the handler ran (no plugin code runs here): the stub of
  -- a saved reply that went in parts, a stub after new parts, or result itself.
  local function deliver(from_hub, result)
    result = result or {}
    local saved, cached = S.stub, S.encoded
    S.stub, S.encoded = nil, nil
    if saved and saved.result == result then return saved.stub end
    check_reserved(result)
    if not from_hub then return result end
    return parted(result, cached and cached.value == result and cached.text or nil) or result
  end
  rex.on('client_perform_requested', function(_, ev)
    -- Fields, not locals (see the note on S at the top). Rex delivers one request at a time.
    S.from_hub = ev.requester ~= nil and ev.requester.client_id == app.hub_id
    S.stub, S.encoded = nil, nil
    local ok, result = pcall(dispatch, ev)
    if ok then ok, result = pcall(deliver, S.from_hub, result) end
    S.from_hub = false
    if ok then raw.call('client.reply', {call_id = ev.call_id, result = result or {}})
    else raw.call('client.reply', {call_id = ev.call_id, error = normalize(result)}) end
  end)

  -- register runs after the main file.
  -- user_objects answers whether the database has a schema object that is not SQLite's or the hub's
  -- (the names that start with lazyrex_, any case: docs/sql.md).
  local function user_objects()
    for _, row in ipairs(db.query("SELECT name FROM sqlite_master WHERE name NOT LIKE 'sqlite!_%' ESCAPE '!'")) do
      if tostring(row.name):lower():sub(1, 8) ~= 'lazyrex_' then return true end
    end
    return false
  end
  local function migrate()
    if #migrations == 0 then return end
    local last = migrate_base + #migrations - 1
    run({tx = true}, function()
      local have = tonumber((db.first('PRAGMA user_version') or {}).user_version) or 0
      -- Expand/contract (docs/sql.md section 1): a newer schema is a rollback to this release. Run on.
      if have > last then
        print('lazyrex: the database has schema ' .. have .. '; this plugin knows ' .. last .. '; running on (expand-only rule, docs/sql.md)')
      end
      -- The base runs only on a new database: version 0 and no schema object of the plugin. It
      -- never runs over a part of the old steps or over data (hub lead, 2026-10-22).
      if migrate_base > 1 and have < migrate_base and (have > 0 or user_objects()) then
        M.fail('internal', 'The database has schema ' .. have .. (have == 0 and ' with tables' or '') .. '; this plugin starts at base '
          .. migrate_base .. '. Start the release that has steps ' .. (have + 1) .. ' to ' .. migrate_base .. ' first (docs/plugin-sdk.md, "Migrations")')
      end
      for v = math.max(have, migrate_base - 1) + 1, last do
        local step = migrations[v - migrate_base + 1]
        if type(step) == 'function' then step(db) else db.exec(step) end
      end
      if have < last then db.exec('PRAGMA user_version = ' .. last) end
    end)
  end
  -- The hub refuses hub.sql from an instance that is not registered (hub/sql 07049a7, observed by
  -- Profiles in a lab), so the registration comes first. Then the migrations and the start code run in
  -- the same timer callback: Rex runs one handler at a time, so a call that arrives meanwhile waits
  -- until they end (docs/rex-protocol.md).
  function S.register()
    local keymap, exports, seen = {}, {}, {}
    local function hub_call(n)
      if seen[n] then return end
      seen[n] = true
      keymap[#keymap + 1] = {name = n, kind = 'client', title = n, args = {type = 'object', additionalProperties = true}}
    end
    for _, n in ipairs({'view.render', 'view.action', 'view.seen', 'view.draft.save', 'view.send', 'view.history', 'hub.changed'}) do hub_call(n) end
    -- hub.prompt: only a plugin that listens (the hub sends it to a plugin with the prompts grant).
    if listeners['hub.prompt'] then hub_call('hub.prompt') end
    -- Each view.* handler name of an app.view (for example view.open, view.rename).
    for _, v in ipairs(views) do
      local names = {}
      for n in pairs(v.handlers) do if type(n) == 'string' and n:sub(1, 5) == 'view.' then names[#names + 1] = n end end
      table.sort(names)
      for _, n in ipairs(names) do hub_call(n) end
    end
    for _, n in ipairs(order) do
      local a = actions[n]
      -- The hub calls a user-only action with {user, operation_key, args}; the SDK checks args itself.
      local wire = a.opts.user_only and {type = 'object', additionalProperties = true} or a.schema
      keymap[#keymap + 1] = {name = n, kind = 'client', title = a.opts.title or n, description = a.opts.description, args = wire}
      if not a.opts.user_only then exports[#exports + 1] = n end
    end
    if #exports > 0 then -- only a plugin with agent actions takes hub.forward
      keymap[#keymap + 1] = {name = 'hub.forward', kind = 'client', title = 'Forward an agent action',
        args = {type = 'object', properties = {action = {type = 'string'}, args = {type = 'object'}, requester_pid = {type = 'integer'}},
          required = {'action', 'args', 'requester_pid'}, additionalProperties = false}}
    end
    raw.call('client.keymap', {actions = keymap})
    local reg = {plugin = name, instance_id = app.instance_id}
    if #exports > 0 then reg.exports = exports end
    if #commands > 0 then reg.commands = commands end
    -- app.registration: the hub.register reply (for example its generation), before the start functions.
    app.registration = hub('hub.register', reg)
    -- The reply limits (inline_max, part_bytes, max_total) have one source: the hub. Each is an integer
    -- from 1 to 2^53 (the range check also refuses NaN and infinity), and inline_max is at most max_total.
    local limits = type(app.registration.limits) == 'table' and app.registration.limits or {}
    local hint = ' (the hub is older than 4.2, or a test sandbox must answer hub.register with'
      .. " dofile(H.args.sdk .. '/testing.lua').register_reply())"
    for _, k in ipairs({'inline_max', 'part_bytes', 'max_total'}) do
      local v = limits[k]
      if type(v) ~= 'number' or not (v >= 1 and v <= 2 ^ 53) or v ~= math.floor(v) then
        fail('internal', 'The hub.register reply has no valid limits.' .. k .. hint)
      end
    end
    if limits.inline_max > limits.max_total then
      fail('internal', 'The hub.register reply has no valid limits: inline_max is over max_total' .. hint)
    end
    raw.call('client.set_label', {label = 'lazyrex-plugin:' .. name})
    migrate()
    for _, s in ipairs(starts) do if not s.opts.after_register then run({tx = s.opts.tx ~= false}, s.fn) end end
    for _, s in ipairs(starts) do if s.opts.after_register then run({tx = s.opts.tx ~= false}, s.fn) end end
  end
  rex.after(0, function()
    local ok, err = pcall(S.register)
    if not ok then
      print('lazyrex: the start of ' .. name .. ' failed: ' .. normalize(err).message)
      os.exit(1)
    end
  end)
  return app
end

-- For tests: the module state.
M._state = S
return M

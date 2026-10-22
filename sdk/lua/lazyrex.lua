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

local MAX_REQUEST = 64 * 1024 -- Rex refuses a larger client.perform request (observed, docs/sql.md section 5).
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
  return table.concat(out)
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
  return table.concat(out)
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
function M.fingerprint(v) return 'v1/' .. digest(table.concat(canonical(v, {}))) end

-- Hub calls --------------------------------------------------------------------------------------

-- rex_error raises a Rex or hub error that came as text. The Rex text ends with
-- "(status=<n> code=<code>)" (observed on Rex 0.0.1792158932-g88fd41). A hub.sql error message
-- starts with "<code>: " (handoff v1, C). Only an error with no code, or with unavailable,
-- disconnected or timeout, is unavailable: the outcome is unknown, and the caller can retry.
local function rex_error(err, default)
  if type(err) == 'table' then fail(err.code or 'unavailable', err.message or default) end
  local text = tostring(err or default)
  local code = text:match('code=([%w_]+)%)$')
  local message = text:match('failed: (.-) %(status=%d+ code=[%w_]+%)$') or text
  local inner, rest = message:match('^([%l_]+): (.*)$')
  if inner and (code == nil or code == 'internal' or code == 'failed' or code == inner) then
    code, message = inner, rest -- a hub code: hub.sql timeout means rolled back (handoff v1, C)
  elseif code == 'timeout' then
    code = nil -- a Rex timeout: the outcome is unknown
  end
  if not code or code == 'disconnected' then fail('unavailable', text) end
  fail(code, message)
end
M.rex_error = rex_error

local function hub(action, args)
  local app = S.app
  local request = {client_id = app.hub_id, action = action, args = args or {}, timeout_ms = 10000}
  local ok, encoded = pcall(rex.json.encode, request)
  if ok and #encoded > MAX_REQUEST then fail('too_large', action .. ': the request has ' .. #encoded .. ' bytes; the limit is ' .. MAX_REQUEST .. '. Nothing was sent.') end
  local response, err = rex.call('client.perform', request)
  if type(response) ~= 'table' then rex_error(err, action .. ' failed') end
  if type(response.result) ~= 'table' then fail('internal', action .. ': invalid hub result') end
  return response.result
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
-- (default true). It commits at the end: with key and store, the reply is saved in the same commit,
-- unless it is a running reply. An error rolls back and raises again.
local function run(spec, fn, ...)
  local f = {tx = spec.tx ~= false, render = spec.render, notices = {}}
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
      if not f.token then f.token = hub('hub.sql.begin', {}).tx end
      local args = {tx = f.token}
      if save then args.key, args.fingerprint, args.reply = spec.key, spec.fingerprint, result or {} end
      if #f.notices > 0 then args.notices = f.notices end
      local done = hub('hub.sql.commit', args)
      if done.replayed then out.result = done.reply end
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
-- db.exec(sql, ...) runs one statement. It answers {changes, id}, and rows for RETURNING.
function db.exec(sql, ...)
  if type(sql) ~= 'string' then fail('invalid_input', 'db.exec needs SQL text') end
  local r = sql_call('hub.sql.exec', {sql = sql, params = params(select('#', ...), ...)})
  local out = {changes = r.changes, id = decode_value(r.id)}
  if r.columns then out.rows = rows_of(r) end
  return out
end
-- db.query(sql, ...) answers a list of rows keyed by column name, and true when more rows exist
-- than one reply holds (paging; docs/sql.md section 4).
function db.query(sql, ...)
  if type(sql) ~= 'string' then fail('invalid_input', 'db.query needs SQL text') end
  local r = sql_call('hub.sql.query', {sql = sql, params = params(select('#', ...), ...)})
  return rows_of(r), r.complete == false
end
-- db.first(sql, ...) answers the first row, or nil.
function db.first(sql, ...) return (db.query(sql, ...))[1] end
-- db.batch(statements) runs {sql, ...values} statements in one transaction, all or nothing. In a
-- handler transaction it runs inside it.
function db.batch(statements)
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
      if type(v) == 'string' and p.minLength and #v < p.minLength then fail('invalid_input', 'Argument ' .. name .. ' is too short') end
      if type(v) == 'string' and p.maxLength and #v > p.maxLength then fail('invalid_input', 'Argument ' .. name .. ' is too long') end
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

local function slug(text) return (tostring(text):lower():gsub('[^%w]+', '-'):gsub('^%-+', ''):gsub('%-+$', '')) end
local function part(s) s = tostring(s); return #s .. ':' .. s end

-- M.plugin(name) answers the app. Call it once, in the main file. The SDK registers the plugin
-- after the main file has run (rex.after 0): first the migrations, then the start functions, then
-- client.keymap and hub.register.
function M.plugin(name)
  assert(S.app == nil, 'lazyrex.plugin is called once')
  assert(type(name) == 'string' and name:match('^[%l%d_%-]+$'), 'A plugin name is a-z, 0-9, _ and -')
  local args = rex.args or {}
  local app = {name = name, hub_id = assert(args.hub_client_id, 'hub_client_id is required (the hub starts the plugin)'),
    instance_id = assert(args.instance_id, 'instance_id is required'), session_id = args.plugin_session or rex.session_id,
    args = {}, db = db, ui = {}, fail = fail, int = M.int, blob = M.blob, route = M.route}
  for k, v in pairs(args) do
    if k ~= 'hub_client_id' and k ~= 'instance_id' and k ~= 'plugin_session' and k ~= 'lazyrex_sdk' and k ~= 'main' then app.args[k] = v end
  end
  app.dir = args.main and args.main:match('^(.*)/[^/]*$')
  S.app = app
  local actions, order, panels, views, commands, starts, migrations = {}, {}, {}, {}, {}, {}, {}
  local listeners = {}

  -- app.hub(action, args) calls the hub. A hub error raises {code, message}: a refusal stays final.
  function app.hub(action, a) return hub(action, a) end
  -- app.rex(method, args) calls Rex. A Rex error raises {code, message}.
  function app.rex(method, a)
    local r, err = rex.call(method, a or {})
    if r == nil then rex_error(err, method .. ' failed') end
    return r
  end
  -- app.refresh() renders every open view of the plugin again (state outside SQL).
  function app.refresh() pcall(hub, 'hub.ui.invalidate', {}) end
  -- app.notice(agent, pending) wakes agent at the commit of the current handler.
  function app.notice(agent, pending)
    local f = frame()
    if not f then fail('invalid_input', 'app.notice runs in a handler') end
    f.notices[#f.notices + 1] = {agent = agent, pending = pending == true}
  end

  -- app.db.migrate {sql, ...}: each entry is one schema version (a SQL text or a function). The
  -- SDK runs the new ones in order, in one transaction, and sets PRAGMA user_version.
  function db.migrate(list)
    for _, step in ipairs(list) do migrations[#migrations + 1] = step end
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

  -- app.panel(title, fn, opts): a tab in the LazyRex window. fn(view) answers a list of nodes
  -- (app.ui.*). opts: id (default from the title), description, keywords, landing (default true).
  function app.panel(title, fn, opts)
    opts = opts or {}
    local id = opts.id or slug(title)
    local route = '/panels/' .. id
    panels[route] = {title = title, fn = fn}
    commands[#commands + 1] = {id = id, title = title, description = opts.description or ('Show ' .. title), keywords = opts.keywords or {id},
      route = route, surface = 'panel', landing = opts.landing ~= false, user_only = 'navigation'}
  end

  -- app.view(route, handlers, opts): a raw view. A route that ends with '*' matches each route with
  -- that prefix. handlers.render(ctx, args) answers the whole tree. handlers[<hub view action>]
  -- (view.action, view.draft.save, view.send, view.seen, ...) get (ctx, args) in one transaction
  -- with ctx.key = the operation key. opts.store = false saves no reply; opts.tx = false runs each SQL
  -- call alone (long operations: use app.db.transaction for the steps).
  function app.view(route, handlers, opts)
    views[#views + 1] = {route = route, prefix = route:sub(-1) == '*' and route:sub(1, -2) or nil, handlers = handlers, opts = opts or {}}
  end
  -- app.command(spec): an extra hub.register command (a palette entry).
  function app.command(spec) commands[#commands + 1] = spec end

  -- app.form(route, spec): a form view (form v1: text, multiline, boolean and select fields). spec:
  --   title: text, or fn(ctx) -> text.
  --   fields: a list, or fn(ctx) -> list of {id, label, kind, value, required, options, ...}. The
  --     function runs at each render and again at the submit, in its transaction.
  --   intro: text, or fn(ctx) -> text: a widget above the form.
  --   submit: {label, tool = <agent action> | user_only = <reason>, enabled = bool | fn(ctx)}.
  --   on_submit(ctx, values) -> result. nil closes the form; a table passes through ({form},
  --     {navigate}, {close_form}, {status = 'running'}, ...).
  --   cancel: the Cancel label (default 'Cancel'); false: no Cancel.
  --   command: a palette command that opens the form ({id, title, description, keywords}).
  --   tx, store: as for app.action.
  -- An app.fail in title, fields or intro shows the message in place of the form. The SDK checks
  -- the values first: a required field must be filled in (a required boolean must be true), a
  -- select value must be an option, and an unknown field is refused.
  function app.form(route, spec)
    local function eval(v, ctx) if type(v) == 'function' then return v(ctx) end return v end
    local function render_form(ctx)
      -- out.title, not a local: a closure must not reassign a captured local (see S at the top).
      local nodes, out = {}, {title = ''}
      local ok, err = pcall(function()
        out.title = tostring(eval(spec.title, ctx) or '')
        local intro = eval(spec.intro, ctx)
        if intro ~= nil and intro ~= '' then nodes[#nodes + 1] = {id = 'intro', type = 'widget', text = tostring(intro)} end
        local submit = spec.submit or {}
        local action = {name = submit.name or 'submit', label = submit.label or 'Submit', enabled = eval(submit.enabled, ctx) ~= false}
        if submit.tool then action.tool = submit.tool else action.user_only = submit.user_only or 'navigation' end
        nodes[#nodes + 1] = {id = 'form', type = 'form', fields = eval(spec.fields, ctx) or {}, submit_action = action}
      end)
      if not ok then nodes = {{id = 'error', type = 'error', text = normalize(err).message}} end
      if spec.cancel ~= false then
        nodes[#nodes + 1] = {id = 'cancel', type = 'widget', text = '', action = {name = '$cancel', label = spec.cancel or 'Cancel', enabled = true}}
      end
      return {title = out.title, nodes = nodes, status = {state = 'blocked', kind = 'question', msg = 'Waiting for input'}}
    end
    local function checked(fields, values)
      local known, out = {}, {}
      for _, f in ipairs(fields or {}) do
        known[f.id] = true
        local v = values[f.id]
        local label = f.label or f.id
        local empty = v == nil or v == '' or (f.kind == 'boolean' and v ~= true)
        if f.required and empty then fail('invalid_input', 'Fill in ' .. label) end
        if v ~= nil then
          local want = f.kind == 'boolean' and 'boolean' or 'string'
          if type(v) ~= want then fail('invalid_input', label .. ' must be ' .. want) end
          if f.kind == 'select' and v ~= '' then
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
        if a.node_id ~= 'form' or a.action ~= (submit.name or 'submit') then fail('invalid_input', 'Unknown form action') end
        if eval(submit.enabled, ctx) == false then fail('conflict', 'This form cannot be sent now. It shows the current state.') end
        local values = type(ctx.input.values) == 'table' and ctx.input.values or {}
        local result = spec.on_submit(ctx, checked(eval(spec.fields, ctx), values))
        if result == nil then return {close_form = true} end
        return result
      end,
    }, {tx = spec.tx, store = spec.store})
    if spec.command then
      local c = copy(spec.command)
      c.route = c.route or route
      if c.action == nil and (spec.submit or {}).tool then c.action = spec.submit.tool end
      commands[#commands + 1] = c
    end
  end
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
  -- session_id) or the hub calls hub.changed and view.seen (fn(args)).
  function app.on(event, fn, opts)
    if event == 'hub.changed' or event == 'view.seen' then
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

  -- tree answers the view tree of a panel: functions are dropped, actions added. An empty list
  -- becomes a widget: Rex Lua cannot send an empty JSON array.
  local function tree(panel, nodes)
    local out = {}
    for i, n in ipairs(nodes or {}) do
      local node = {id = tostring(n.id), type = n.type, label = n.label, text = n.text, action = action_of(n)}
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
    return {user = true, key = a.operation_key, route = a.route, input = type(a.input) == 'table' and a.input or {}, args = a,
      session_id = context.session_id, view_id = a.view_id, context = context, query = query_of(a.route)}
  end
  local function view_call(action, a)
    local panel = panels[a.route]
    if action == 'view.render' then
      if panel then
        return {tree = run({tx = false, render = a.render}, function() return tree(panel, panel.fn(user_ctx(a))) end)}
      end
      local v = view_of(a.route)
      if v and v.handlers.render then return {tree = run({tx = false, render = a.render}, v.handlers.render, user_ctx(a), a)} end
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
    if ev.action == 'hub.changed' then
      for _, l in ipairs(listeners['hub.changed'] or {}) do run({tx = l.opts.tx ~= false}, l.fn, a) end
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

  rex.on('client_perform_requested', function(_, ev)
    local ok, result = pcall(dispatch, ev)
    if ok then rex.call('client.reply', {call_id = ev.call_id, result = result or {}})
    else rex.call('client.reply', {call_id = ev.call_id, error = normalize(result)}) end
  end)

  -- register runs after the main file.
  local function migrate()
    if #migrations == 0 then return end
    run({tx = true}, function()
      local have = tonumber((db.first('PRAGMA user_version') or {}).user_version) or 0
      if have > #migrations then fail('refused', 'The database has schema version ' .. have .. '; this plugin knows ' .. #migrations) end
      for i = have + 1, #migrations do
        local step = migrations[i]
        if type(step) == 'function' then step(db) else db.exec(step) end
      end
      if have < #migrations then db.exec('PRAGMA user_version = ' .. #migrations) end
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
    rex.call('client.keymap', {actions = keymap})
    local reg = {plugin = name, instance_id = app.instance_id}
    if #exports > 0 then reg.exports = exports end
    if #commands > 0 then reg.commands = commands end
    hub('hub.register', reg)
    rex.call('client.set_label', {label = 'lazyrex-plugin:' .. name})
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

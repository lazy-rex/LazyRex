-- The Lua test API for LazyRex plugins (docs/testing.md). runner.lua loads it and makes its
-- functions global. Tests run against a real hub on an isolated lab server that `lazyrex-hub test`
-- owns.
--
-- Rules that this module keeps:
--   * The runner never answers a callback, so it never waits for one: it polls hub state that
--     the hub returns at once (hub.test.*), with short sleeps and a deadline.
--   * Agents and the UI user are real panes in the lab (testagent, testview), so the hub sees real
--     pane ancestry for their calls.
--   * One control message is at most 64 KiB (rex-protocol.md, "Rex Lua runtime"). Requests are
--     measured before they leave; large records and trees arrive in chunks.

local H = {}

local A        -- rex.args
local self_id  -- this runner's Rex client ID
local hub_id   -- the hub's client ID
local REQUEST_LIMIT = 65536 - 1024 -- room for the frame id and the server's own fields

-- --- Errors ---

local Err = {}
Err.__index = Err
Err.__tostring = function(e)
  return (e.code and e.code ~= '' and (e.code .. ': ') or '') .. tostring(e.message)
end

local function err_of(text)
  if type(text) == 'table' then return setmetatable(text, Err) end
  text = tostring(text)
  -- `stream response "control-1" failed: <message> (status=500 code=<code>)` (rex-protocol.md)
  local code = text:match('code=([%w_%-%.]+)%)%s*$')
  local msg = text:match('failed: (.*) %(status=%d+ code=[^)]*%)%s*$') or text
  -- A Rex error message keeps its own "code: " prefix; drop it when it repeats the code.
  if code and msg:sub(1, #code + 2) == code .. ': ' then msg = msg:sub(#code + 3) end
  return setmetatable({code = code, message = msg, raw = text}, Err)
end

H.is_error = function(v) return getmetatable(v) == Err end

-- --- Rex calls ---

local function measure(method, payload)
  local frame = rex.json.encode({type = 'request', id = 'control-0000000000', method = method, payload = payload})
  if #frame > REQUEST_LIMIT then
    error(setmetatable({code = 'request_too_large', message = string.format(
      '%s request is %d bytes encoded; one control message is at most 65536', method, #frame)}, Err), 0)
  end
end

local function perform(client, action, args, timeout_ms)
  -- A missing client would reach Rex as "" and fail there without the caller's place.
  if type(client) ~= 'string' or client == '' then
    error(setmetatable({code = 'bad_client', message = string.format('%s: the client ID is %q, not a client ID\n%s',
      tostring(action), tostring(client), debug.traceback())}, Err), 0)
  end
  local payload = {client_id = client, action = action, args = args or {}, timeout_ms = timeout_ms or 10000}
  measure('client.perform', payload)
  local res, err = rex.call('client.perform', payload)
  if err ~= nil or res == nil then error(err_of(err or 'no result'), 0) end
  return res.result
end
H.perform = perform

-- H.rex_call is the Rex call of resolve. A harness test replaces it with a fake Rex.
H.rex_call = function(method, payload) return rex.call(method, payload) end

-- H.diag writes a diagnostic line to the runner log (stdout lines without "LRT " go there).
H.diag = function(text) io.write('harness: ', (text:gsub('\n', ' ')), '\n') end

local function is_client_id(id) return type(id) == 'string' and id:sub(1, 7) == 'client:' and #id > 7 end

-- resolve gives the one connected client with this label, or nil. A candidate counts only when its
-- ID is a client ID and client.inspect answers for that same ID, connected. Every other candidate
-- is logged with the raw target.resolve answer and the inspect answer or error (a refusal can come
-- back as an error or be raised): it is the evidence for a View with an empty client (the Worktrees
-- flake, 2026-10-22). Only a well-formed client that inspect reports disconnected is skipped
-- without a line: a client that closed.
local function resolve(label)
  local res, err = H.rex_call('target.resolve', {kinds = {'client'}, queries = {label}})
  if err or not res or not res.results or not res.results[1] then return nil end
  local r = res.results[1]
  local cands = r.status == 'resolved' and {r.target} or (r.candidates or {})
  local connected = {}
  for _, c in ipairs(cands) do
    -- The answer goes into a table: this pcall gives only its first result back.
    local got = {}
    local ok, raised = pcall(function() got.ins, got.err = H.rex_call('client.inspect', {client_id = c.client_id}) end)
    local ins, ierr = got.ins, got.err
    if not ok then ins, ierr = nil, raised end
    local cl = type(ins) == 'table' and ins.client or nil
    if is_client_id(c.client_id) and cl and cl.client_id == c.client_id then
      if cl.connection_state == 'connected' then connected[#connected + 1] = c.client_id end
    else
      local answer = ierr ~= nil and ('error ' .. rex.json.encode(ierr)) or rex.json.encode(ins)
      H.diag(string.format('resolve %s: refused candidate %q: target.resolve %s; client.inspect %s', label,
        tostring(c.client_id), rex.json.encode(res), answer))
    end
  end
  if #connected == 1 then return connected[1] end
  return nil
end
H.resolve = resolve

-- H.check_client is the last check of a View's client: an error, never a View that cannot be called.
H.check_client = function(v, client)
  if not is_client_id(client) then
    error(setmetatable({code = 'bad_client', message = string.format('view %s (%s, label %s): the renderer client is %q, not a client ID',
      tostring(v.id), tostring(v.route), tostring(v.label), tostring(client))}, Err), 0)
  end
  return client
end

local function hub(action, args) return perform(hub_id, action, args) end
H.hub = hub

-- wait polls fn until it returns a value other than nil or false. It raises after timeout seconds
-- with what, and with the last error fn raised. Callers read the return value; a callback that
-- assigns an outer local can lose the write after a caught error (docs/testing.md, "Waiting").
local function wait(what, fn, timeout)
  local deadline = os.time() + (timeout or 10)
  local delay, last = 0.02, nil
  while true do
    local ok, v = pcall(fn)
    if ok and v ~= nil and v ~= false then return v end
    if not ok then last = v end
    if os.time() >= deadline then
      error(setmetatable({code = 'timeout', message = 'timed out waiting for ' .. what ..
        (last and (': ' .. tostring(last)) or '')}, Err), 0)
    end
    rex.sleep(delay)
    delay = math.min(delay * 2, 0.2)
  end
end
H.wait = wait

-- --- Values ---

local function is_array(t)
  local n = 0
  for _ in pairs(t) do n = n + 1 end
  for i = 1, n do if t[i] == nil then return false end end
  return true
end

local function show(v, depth)
  depth = depth or 0
  if type(v) == 'string' then return string.format('%q', v) end
  if type(v) ~= 'table' or H.is_error(v) then return tostring(v) end
  if depth > 3 then return '{…}' end
  -- The text is cut at 600 characters, so parts stop there: Rex Lua's table.concat fails on more
  -- than 2559 items ("registry overflow", observed on Rex 0.0.1792158932-g88fd41).
  local parts, n = {}, 0
  local function add(text)
    if n > 600 then return false end
    parts[#parts + 1] = text
    n = n + #text + 2
    return true
  end
  if is_array(v) then
    for _, x in ipairs(v) do if not add(show(x, depth + 1)) then break end end
  else
    local keys = {}
    for k in pairs(v) do keys[#keys + 1] = k end
    table.sort(keys, function(a, b) return tostring(a) < tostring(b) end)
    for _, k in ipairs(keys) do if not add(tostring(k) .. '=' .. show(v[k], depth + 1)) then break end end
  end
  local s = '{' .. table.concat(parts, ', ') .. '}'
  if #s > 600 then s = s:sub(1, 600) .. '…' end
  return s
end
H.show = show

-- equal compares deeply. An empty table equals an empty table: Rex Lua decodes [] and {} alike.
local function equal(a, b)
  if type(a) ~= type(b) then return false end
  if type(a) ~= 'table' then return a == b end
  for k, v in pairs(a) do if not equal(v, b[k]) then return false end end
  for k in pairs(b) do if a[k] == nil then return false end end
  return true
end
H.equal = equal

-- fail raises without a position: the runner adds the line in the test file (runner.lua, where).
local function fail(msg)
  error(msg, 0)
end

-- --- Assertions ---

local Expect = {}
Expect.__index = Expect

-- expect(v).to_be(x) and expect(v):to_be(x) both work: each method is bound to its subject, and a
-- colon call's extra first argument is dropped.
function H.expect(v, label)
  local e = setmetatable({v = v, label = label}, Expect)
  local bound = {}
  for name, fn in pairs(Expect) do
    if type(fn) == 'function' and name ~= '__index' then
      bound[name] = function(first, ...)
        if first == bound then return fn(e, ...) end
        return fn(e, first, ...)
      end
    end
  end
  return bound
end

-- what returns the label prefix of a failure. A label that is not text is shown, so the real
-- failure stays visible.
local function what(e)
  if e.label == nil then return '' end
  return (type(e.label) == 'string' and e.label or show(e.label)) .. ': '
end

function Expect:to_equal(x)
  if not equal(self.v, x) then fail(what(self) .. 'expected ' .. show(x) .. ', got ' .. show(self.v), 2) end
  return self
end
function Expect:to_be(x)
  if self.v ~= x then fail(what(self) .. 'expected ' .. show(x) .. ', got ' .. show(self.v), 2) end
  return self
end
function Expect:not_to_be(x)
  if self.v == x then fail(what(self) .. 'expected anything but ' .. show(x), 2) end
  return self
end
function Expect:to_be_truthy()
  if not self.v then fail(what(self) .. 'expected a true value, got ' .. show(self.v), 2) end
  return self
end
function Expect:to_be_falsy()
  if self.v then fail(what(self) .. 'expected nil or false, got ' .. show(self.v), 2) end
  return self
end
function Expect:to_match(pattern)
  if type(self.v) ~= 'string' or not self.v:find(pattern) then
    fail(what(self) .. 'expected a string matching ' .. show(pattern) .. ', got ' .. show(self.v), 2)
  end
  return self
end
function Expect:to_contain(x)
  local found = false
  if type(self.v) == 'string' then found = type(x) == 'string' and self.v:find(x, 1, true) ~= nil
  elseif type(self.v) == 'table' then
    for _, y in pairs(self.v) do if equal(y, x) then found = true end end
  end
  if not found then fail(what(self) .. 'expected ' .. show(self.v) .. ' to contain ' .. show(x), 2) end
  return self
end
function Expect:not_to_contain(x)
  local found = false
  if type(self.v) == 'string' then found = type(x) == 'string' and self.v:find(x, 1, true) ~= nil
  elseif type(self.v) == 'table' then
    for _, y in pairs(self.v) do if equal(y, x) then found = true end end
  end
  if found then fail(what(self) .. 'expected ' .. show(self.v) .. ' not to contain ' .. show(x), 2) end
  return self
end
function Expect:to_have_length(n)
  local len = 0
  if type(self.v) == 'table' then for _ in pairs(self.v) do len = len + 1 end
  elseif type(self.v) == 'string' then len = #self.v end
  if len ~= n then fail(what(self) .. 'expected length ' .. n .. ', got ' .. len .. ' in ' .. show(self.v), 2) end
  return self
end

function H.assert_eq(a, b, msg)
  if not equal(a, b) then fail((msg and (msg .. ': ') or '') .. 'expected ' .. show(b) .. ', got ' .. show(a), 2) end
end
function H.assert_true(v, msg)
  if not v then fail(msg or 'expected a true value', 2) end
end

-- expect_error runs fn and returns its error. pattern matches the message or equals the code.
function H.expect_error(fn, pattern)
  local ok, e = pcall(fn)
  if ok then fail('expected an error' .. (pattern and (' matching ' .. show(pattern)) or '') .. ', got success: ' .. show(e), 2) end
  e = H.is_error(e) and e or err_of(e)
  if pattern and e.code ~= pattern and not tostring(e.message):find(pattern) and not tostring(e.raw or ''):find(pattern) then
    fail('expected an error matching ' .. show(pattern) .. ', got ' .. tostring(e) .. (e.raw and (' [' .. e.raw .. ']') or ''), 2)
  end
  return e
end

-- --- Hub state (hub.test.*) ---

local function chunked(source, fields)
  local parts, offset = {}, 0
  while true do
    local a = {source = source, offset = offset}
    for k, v in pairs(fields) do a[k] = v end
    local c = hub('hub.test.chunk', a)
    parts[#parts + 1] = c.text
    if c.done then break end
    offset = c.next
  end
  return table.concat(parts)
end

-- store returns {value, rev} of one record of the plugin (default: the plugin under test). An
-- absent record is {rev = '0'}. A large record is read in chunks of one revision; if it changes
-- during the read, the read starts again.
function H.store(key, plugin)
  plugin = plugin or A.plugin
  for _ = 1, 5 do
    local out = hub('hub.test.store', {plugin = plugin, keys = {key}})
    if out.records and out.records[key] then return out.records[key] end
    local big = out.too_large and out.too_large[key]
    if not big then fail('store: no answer for ' .. show(key), 2) end
    local ok, text = pcall(chunked, 'record', {plugin = plugin, key = key, rev = big.rev})
    if ok then return {value = rex.json.decode(text), rev = big.rev} end
    if not (H.is_error(text) and text.code == 'changed') and not tostring(text):find('changed') then error(text, 0) end
  end
  fail('store: ' .. key .. ' kept changing during a chunked read', 2)
end

function H.notice(agent, plugin) return hub('hub.test.notice', {plugin = plugin or A.plugin, agent = agent}) end
function H.batches(plugin) return hub('hub.test.stats', {plugin = plugin or A.plugin}).batches end

-- invalidates counts the plugin's hub.ui.invalidate calls. A count that stays the same over an
-- interval proves only that the plugin made no such call in that interval.
function H.invalidates(plugin) return hub('hub.test.stats', {plugin = plugin or A.plugin}).invalidates end
-- calls returns the plugin's recorded hub service calls of action ({seq, action, instance_id,
-- args}, oldest first), and the full reply {calls, next, dropped}. opts: after (a seq; only later
-- calls), plugin (default: the plugin under test). A test that proves "never" checks that dropped
-- is false: the hub keeps the last 1000 calls of each plugin.
function H.calls(action, opts)
  opts = opts or {}
  local out = hub('hub.test.calls', {plugin = opts.plugin or A.plugin, action = action, after = opts.after or 0})
  local list = out.calls
  if list == nil or list == rex.json.null then list = {} end
  return list, out
end
-- renders returns the render counts of each view of the plugin (hub.test.renders): {view_id,
-- route, attempts, ok, failed, transient, causes = {commit, invalidate, ...}, declared}, in view_id
-- order. declared is the declaration that the view has now ({prefixes, changes}), nil for none.
function H.renders(plugin)
  local v = hub('hub.test.renders', {plugin = plugin or A.plugin}).views
  if v == nil or v == rex.json.null then return {} end
  return v
end
function H.advance_rev(at) return hub('hub.test.rev', {at = tostring(at)}).rev end

function H.tabs(plugin)
  local t = hub('hub.test.tabs', {plugin = plugin or A.plugin}).tabs
  if t == nil or next(t) == nil then return {} end
  return t
end
function H.tab(key, plugin)
  for _, t in ipairs(H.tabs(plugin)) do if t.key == key then return t end end
  return nil
end

function H.views(plugin)
  local v = hub('hub.test.views', {plugin = plugin or A.plugin}).views
  if v == nil or next(v) == nil then return {} end
  return v
end

function H.operation(key, plugin) return hub('hub.test.operation', {plugin = plugin or A.plugin, operation_key = key}) end

-- inject commits ops (store.batch ops) before the plugin's next batch that matches match
-- ({op_key=}, {op_prefix=} or {writes=key}). spec: {match, ops, replay, times}. Returns its ID.
-- match {sql_commit_writes = table} fails the plugin's next hub.sql commit that changed that
-- table: spec.fail (and fail_code); with after = true the commit happens, and the plugin gets the
-- error (a lost reply).
function H.inject(spec)
  local a = {plugin = spec.plugin or A.plugin, match = spec.match, ops = spec.ops, replay = spec.replay, times = spec.times,
    fail = spec.fail, fail_code = spec.fail_code, skip = spec.skip, hold = spec.hold, after = spec.after, sql = spec.sql}
  return hub('hub.test.inject', a).id
end
function H.injection(id) return hub('hub.test.injection', {id = id}) end
-- cancel_injection ends an injection (for example times = 'always'): it matches no later batch.
function H.cancel_injection(id) return hub('hub.test.inject.cancel', {id = id}) end

-- fault arms a one-shot hub fault (docs/testing.md, "Faults"): {point = 'spawn-create', mode, op}
-- or {point = 'action', action, when, mode, key, plugin}. It returns the fault id. A fault that
-- has not fired at the end of the file fails the file.
function H.fault(spec)
  local a = {plugin = spec.plugin or A.plugin}
  for k, v in pairs(spec) do if k ~= 'plugin' then a[k] = v end end
  return hub('hub.test.fault', a).id
end
function H.fault_fired(id) return hub('hub.test.fault_state', {id = id}).fired end
-- fault_release opens a gate fault that has fired: later matching calls run. A gate that is still
-- closed at the end of the file fails the file.
function H.fault_release(id) return hub('hub.test.fault_release', {id = id}) end

-- with_injection runs fn with an injection and checks that exactly n batches consumed it.
function H.with_injection(spec, n, fn)
  local id = H.inject(spec)
  local r = table.pack and table.pack(fn()) or {fn()}
  local got = H.injection(id).consumed
  if got ~= n then fail(string.format('injection %d: expected %d matching batches, got %d', id, n, got), 2) end
  return (unpack or table.unpack)(r)
end

-- write commits store ops in the plugin's namespace now, as another writer: {key, value} or
-- {key, delete = true}, each with an optional if_rev (a stale one fails with the code conflict).
function H.write(ops, plugin) return hub('hub.test.write', {plugin = plugin or A.plugin, ops = ops}) end

-- sql_rows turns a hub.sql query reply into rows keyed by column name: NULL is nil, {int} is a
-- decimal string, {blob} stays a {blob = base64} table.
local function sql_rows(out)
  local rows = {}
  for i, r in ipairs(out.rows or {}) do
    local row = {}
    for j, c in ipairs(out.columns or {}) do
      local v = r[j]
      if type(v) == 'table' and v.null then v = nil elseif type(v) == 'table' and v.int then v = v.int end
      row[c] = v
    end
    rows[i] = row
  end
  return rows
end

-- sql reads the plugin's SQL database (the committed state): rows keyed by column name. params is
-- a list for ? or a table for :name; a NULL parameter is {null = true}.
function H.sql(sql, params, plugin)
  return sql_rows(hub('hub.test.sql', {plugin = plugin or A.plugin, sql = sql, params = params or {}}))
end

-- sql_write runs one statement on the plugin's SQL database now, as another writer would (for
-- fixtures). It returns {changes, id}. The plugin's views render again.
function H.sql_write(sql, params, plugin)
  return hub('hub.test.sql', {plugin = plugin or A.plugin, sql = sql, params = params or {}, write = true})
end

-- put_operation saves an operation record {key, fingerprint, result}, as an older plugin saved it.
function H.put_operation(op, plugin)
  return hub('hub.test.op.put', {plugin = plugin or A.plugin, key = op.key, fingerprint = op.fingerprint, result = op.result})
end

-- write_profiles replaces the hub's profile file (LAB/profiles.json; it is the only target).
-- profiles is a table, encoded as JSON, or a string, written as it is (for example malformed JSON).
-- Rex Lua encodes an empty table as {}: for no profiles, pass the string {"profiles":[]}.
-- The hub reads the file again on each profile call. In test mode it refuses a profile with a
-- reserved env key or a path outside the lab and plugin directories.
function H.write_profiles(profiles)
  local text = type(profiles) == 'string' and profiles or rex.json.encode(profiles)
  local f, err = io.open(A.profiles, 'w')
  if not f then error('write_profiles: ' .. tostring(err), 0) end
  f:write(text)
  f:close()
end

-- close_session destroys a lab session (for example a thread's home).
function H.close_session(id)
  local _, err = rex.call('session.destroy', {session_id = id})
  if err then error(err_of(err), 0) end
end

-- session creates a real lab session and returns its ID.
function H.session(label)
  local res, err = rex.call('session.create', {label = label, initial_windows = {{window_label = label,
    layout = {block = {flavor = 'com.superlogical.terminal.shell', options = {command = {'/bin/sleep', '100000'}}}}}}})
  if err then error(err_of(err), 0) end
  return res.session_id
end

-- --- Agents (testagent panes) ---

local Agent = {}
Agent.__index = Agent
local agents = {}

-- agent(id, opts) opens a test agent pane once. opts.join: the roster agent to join as (default
-- id; false: no join, for a conversation identity). opts.title: the tab label (default id).
-- opts.session: the session of its pane (default the tests session).
function H.agent(id, opts)
  if agents[id] then return agents[id] end
  opts = opts or {}
  local join = opts.join
  if join == nil then join = id end
  local cmd = {A.hub_bin, 'testagent', '-id', id}
  if join then cmd[#cmd + 1] = '-join'; cmd[#cmd + 1] = join end
  local sess = opts.session or A.tests_session
  -- opts.unnamed: the tab gets no label, so Rex shows its generated label.
  local label = opts.title or id
  if opts.unnamed then label = nil end
  -- opts.keep_open: the pane stays after its process exits (by default Rex closes it on exit,
  -- observed on Rex 0.0.1792158932-g88fd41).
  local options = {command = cmd}
  if opts.keep_open then options.exit = {on_completion = false} end
  local win, err = rex.call('session.new_window', {session_id = sess, window_label = label,
    layout = {block = {flavor = 'com.superlogical.terminal.shell', options = options}}})
  if err then error(err_of(err), 0) end
  local a = setmetatable({id = id, label = 'lazyrex-testagent:' .. id, session = sess, window = win.window_id}, Agent)
  a.client = wait('agent ' .. id .. ' to start', function() return resolve(a.label) end, 15)
  local st = wait('agent ' .. id .. ' to join', function()
    local s = perform(a.client, 'agent.status', {})
    if s.joined or (s.join_error and s.join_error ~= '') then return s end
    return nil
  end, 15)
  if not st.joined then fail('agent ' .. id .. ' could not join: ' .. st.join_error, 2) end
  a.pid = st.pid
  agents[id] = a
  return a
end

-- set_label renames the agent's tab, as the user does in Rex. The hub names an identity by its
-- tab label. An empty label gives the window its generated default label.
function Agent:set_label(label)
  local _, err = rex.call('session.set_window_label', {session_id = self.session, window_id = self.window, label = label})
  if err then error(err_of(err), 0) end
end

-- title sets the terminal title of the agent's pane (OSC 2), as an agent CLI does.
function Agent:title(text) return perform(self.client, 'agent.title', {title = text}) end
-- print writes text to the agent's pane as program output (no newline is added), so a test can
-- read known text on the screen, for example in a terminal snapshot.
function Agent:print(text) return perform(self.client, 'agent.print', {text = text}) end
-- program_status reports the root program status record (OSC 7501, Rex Program Status Protocol)
-- with this state, for example idle, working or blocked. opts: kind, app and msg (plain text), as
-- an agent CLI reports a prompt.
function Agent:program_status(state, opts)
  local args = {state = state}
  for k, v in pairs(opts or {}) do args[k] = v end
  return perform(self.client, 'agent.program_status', args)
end
-- input returns the bytes written to this pane since the first input call (the first call starts
-- the capture: raw mode, no echo). reset = true clears the captured bytes after the read.
function Agent:input(reset) return perform(self.client, 'agent.input', {reset = reset or false}).data end
-- exit ends the agent process, as the user quits an agent CLI. The agent cannot be used again.
function Agent:exit()
  perform(self.client, 'agent.exit', {})
  agents[self.id] = nil
end
-- call runs an exported plugin action through the agent face: the MCP path.
function Agent:call(action, args) return perform(self.client, 'agent.call', {action = action, args = args or {}}) end
-- call_raw is call with the arguments as JSON text. Rex Lua encodes an empty table as {}
-- (observed on Rex 0.0.1792158932-g88fd41), so an empty array needs it: '{"ids":[]}'.
function Agent:call_raw(action, json_text)
  return perform(self.client, 'agent.call', {action = action, args_json = json_text})
end
-- direct calls the plugin client itself (not through the hub).
function Agent:direct(action, args, plugin)
  return perform(self.client, 'agent.direct', {plugin = plugin or A.plugin, action = action, args = args or {}})
end
function Agent:hub(action, args) return perform(self.client, 'agent.hub', {action = action, args = args or {}}) end
-- report sends hub.identity.report from a new process in this agent's pane, like an agent hook.
-- r.block: an optional block hint (the hub ignores a wrong one: the ancestry decides).
local function report_args(r)
  return {kind = r.kind, conversation_id = r.conversation_id, source = r.source or 'startup', block = r.block}
end
function Agent:report(r)
  return perform(self.client, 'agent.report', {args = report_args(r)}, 20000)
end
-- report_async starts a report process that waits delay_ms before it reports, and returns at once.
-- Its start time is older than a report started after it. report_result waits for the outcome:
-- {result = ...} or {error = {code, message}}.
function Agent:report_async(key, r, delay_ms)
  return perform(self.client, 'agent.report', {args = report_args(r), async = key, delay_ms = delay_ms or 0})
end
function Agent:report_result(key)
  return wait('report ' .. key, function()
    local s = perform(self.client, 'agent.report_result', {async = key})
    if s.done then return s end
    return nil
  end, 30)
end

-- --- The UI user (testview renderer panes) ---

local View = {}
View.__index = View
local User = {}
User.__index = User
H.user = setmetatable({}, User)

local function view_of(info)
  local v = setmetatable({id = info.view_id, route = info.route, tab = info.tab, plugin = info.plugin or A.plugin,
    label = 'lazyrex-testview:' .. info.view_id, context = info.context, session = info.session_id, window = info.window_id, block = info.block_id}, View)
  v.client = H.check_client(v, wait('the renderer of ' .. v.route .. ' to attach', function()
    for _, w in ipairs(H.views(v.plugin)) do
      if w.view_id == v.id and w.attached then return resolve(v.label) end
    end
    return nil
  end, 15))
  return v
end

-- open navigates to a tab of the plugin, as the user does, and returns its view.
function User:open(tab, plugin)
  plugin = plugin or A.plugin
  hub('hub.ui.navigate', {plugin = plugin, tab = tab})
  return self:view(tab, plugin)
end

-- view returns the open view of a tab key or a route (a form route, for example). session: only
-- a view in that session (a panel or a form can be open in several sessions).
-- It takes the newest matching view (hub.test.views lists the oldest first) and waits until it is
-- attached. A replaced view can stay open for a moment beside its replacement, and the old one
-- may still be attached; it is about to close, so it is never selected.
function User:view(key, plugin, session)
  plugin = plugin or A.plugin
  local info = wait('an open, attached view of ' .. key, function()
    local found
    for _, w in ipairs(H.views(plugin)) do
      if (w.tab == key or w.route == key or w.route == '/' .. key) and (not session or w.session_id == session) then
        found = w
      end
    end
    if not (found and found.attached) then return nil end
    found.plugin = plugin
    return found
  end, 15)
  return view_of(info)
end

-- command runs a palette command (plugin.id) from a pane, as the Rex palette does.
-- from: a View (the palette opens in that view's pane, so the view is the source), or a block
-- in the tests session (default: its first block).
function User:command(id, from, plugin)
  plugin = plugin or A.plugin
  local session, block = A.tests_session, from
  if type(from) == 'table' then session, block = from.session, from.block end
  if not block then
    local blocks = rex.call('session.list_blocks', {session_id = A.tests_session})
    block = blocks and blocks.blocks and blocks.blocks[1] and blocks.blocks[1].block_id
  end
  return hub('hub.palette.run', {command = plugin .. '.' .. id, ctx = {client_id = self_id, session_id = session, block_id = block}})
end

-- panel runs a palette command (plugin.id) from a new pane, as the user does, and returns the
-- view that the command opens (a panel, or a form). The pane is a sleep pane in a new window of
-- opts.session (default: a new session). opts.plugin: the plugin (default the plugin under test).
-- A command whose open waits for a confirm (status pending_confirmation) is an error.
function User:panel(id, opts)
  opts = opts or {}
  local plugin = opts.plugin or A.plugin
  local sess = opts.session or H.session('panel-' .. id)
  local win, err = rex.call('session.new_window', {session_id = sess, window_label = 'panel-' .. id,
    layout = {block = {flavor = 'com.superlogical.terminal.shell', options = {command = {'/bin/sleep', '100000'}, login = false}}}})
  if err then error(err_of(err), 0) end
  local out = self:command(id, {session = sess, block = win.block_ids[1]}, plugin)
  if out.status ~= 'opened' and out.status ~= 'opening' then
    error('command ' .. plugin .. '.' .. id .. ': the open is ' .. tostring(out.status), 0)
  end
  local info = wait('the view of command ' .. id, function()
    for _, w in ipairs(H.views(plugin)) do
      if w.view_id == out.view_id and w.attached then return w end
    end
    return nil
  end, 15)
  info.plugin = plugin
  return view_of(info)
end

-- plugin_view calls the plugin's view.* action from this runner, which is not the hub: for refusals.
function User:plugin_view(action, args, plugin)
  local st = hub('hub.test.status', {})
  local p = st.plugins[plugin or A.plugin]
  return perform(p.client_id, 'view.' .. action, args or {})
end

-- render asks for a new render and returns the tree. It fails if that render failed, even when
-- the hub kept an older tree.
function View:render()
  local g = hub('hub.test.view.render', {view_id = self.id}).generation
  -- Longer than the hub's 10 s plugin call timeout, so a render that times out is reported.
  local r = wait('render ' .. g .. ' of ' .. self.route, function()
    local x = hub('hub.test.view.result', {view_id = self.id, generation = g})
    if x.status ~= 'pending' then return x end
    return nil
  end, 25)
  if r.status ~= 'ok' then error(setmetatable({code = 'render_failed', message = self.route .. ': ' .. tostring(r.error)}, Err), 0) end
  self.revision = r.revision
  if r.tree then return r.tree end
  return rex.json.decode(chunked('render', {view_id = self.id, generation = r.generation}))
end

-- try_render returns the render result without raising: {status = 'ok', tree} or {status = 'failed', error}.
function View:try_render()
  local ok, t = pcall(function() return self:render() end)
  if ok then return {status = 'ok', tree = t} end
  if H.is_error(t) and t.code == 'render_failed' then return {status = 'failed', error = t.message} end
  error(t, 0)
end

function View:get() return perform(self.client, 'testview.get', {}) end

-- rename_tab renames the view's Rex tab, as the user does. The hub then sends view.rename.
function View:rename_tab(label)
  local _, err = rex.call('session.set_window_label', {session_id = self.session, window_id = self.window, label = label})
  if err then error(err_of(err), 0) end
end
-- tab_label returns the label of the view's Rex tab. (The field label is the renderer client label.)
function View:tab_label()
  local sv, err = rex.call('session.view', {session_id = self.session})
  if err then error(err_of(err), 0) end
  local windows = sv.windows or {}
  for _, w in ipairs(windows) do
    if w.window_id == self.window then return w.label end
  end
  return nil
end
function View:status() return perform(self.client, 'testview.status', {}) end

-- A table field, not a local: a closure write to a captured local can be lost after a catch
-- (docs/rex-protocol.md, "Rex Lua runtime").
local op_seq = {n = 0}
local function new_key(prefix)
  op_seq.n = op_seq.n + 1
  return (prefix or 'op') .. '-' .. os.time() .. '-' .. op_seq.n
end

-- admit sends a view operation and returns the admission ({status = 'accepted'} or the state of an
-- earlier admission with the same key).
function View:admit(kind, a)
  a.operation_key = a.operation_key or new_key(kind)
  local action = ({action = 'testview.action', draft = 'testview.draft', send = 'testview.send'})[kind]
  local res = perform(self.client, action, a)
  return res, a.operation_key
end

-- settle waits until the operation is final and its effects are not pending. It reads the
-- durable record from the hub (hub.test.operation), so it works after a close_form effect closed
-- this renderer.
function View:settle(key, timeout)
  return wait('operation ' .. key, function()
    local s = H.operation(key, self.plugin)
    if s.status == 'pending' or s.effects == 'pending' then return nil end
    return s
  end, timeout)
end

-- action runs a view action through the hub (admission, durable operation, effects) and returns
-- the final operation state {status, result | error, effects, effects_error}.
function View:action(node, action, input, key)
  local _, k = self:admit('action', {node_id = node, action = action, input = input or {}, revision = self.revision, operation_key = key})
  return self:settle(k)
end
function View:draft(node, text, base_revision, key)
  local _, k = self:admit('draft', {node_id = node, text = text, base_revision = base_revision, operation_key = key})
  return self:settle(k)
end
function View:send(node, text, draft_revision, key)
  local _, k = self:admit('send', {node_id = node, text = text, draft_revision = draft_revision, operation_key = key})
  return self:settle(k)
end
-- seen reports that the user looks at this view, as the renderer does on focus (hub.view.seen).
-- The hub sends view.seen to the plugin later, from its view worker. Wait for the result.
function View:seen(reason)
  return perform(self.client, 'testview.seen', {reason = reason or 'focus'})
end
function View:effect_retry(key)
  perform(self.client, 'testview.effect_retry', {operation_key = key})
  return self:settle(key)
end

-- history asks for a page older than before, polling the hub's pending answer.
function View:history(before)
  return wait('history before ' .. before, function()
    local r = perform(self.client, 'testview.history', {before = before})
    if r.status == 'pending' then return nil end
    return r
  end)
end

-- --- Tree helpers ---

function H.node(tree, id)
  local found
  local function walk(t)
    if found or type(t) ~= 'table' then return end
    if t.id == id then found = t; return end
    for _, v in pairs(t) do walk(v) end
  end
  walk(tree)
  return found
end

-- --- Setup ---

-- --- Fake Rex apps (hub.test.app, test mode only) ---

local App = {}
App.__index = App

-- app(name, opts) opens a fake Rex app in the hub process: a client whose hello says kind "app".
-- opts.sessions: the sessions it shows; opts.fail: an error code it answers instead of {}. It
-- returns when client.list shows the app connected with those sessions.
function H.app(name, opts)
  opts = opts or {}
  hub('hub.test.app', {name = name, sessions = opts.sessions or {}, fail = opts.fail or ''})
  local a = setmetatable({name = name}, App)
  for _, s in ipairs(opts.sessions or {}) do a:wait_shows(s) end
  return a
end

function App:wait_shows(session)
  wait('the fake app ' .. self.name .. ' to show ' .. session, function()
    for _, c in ipairs(rex.call('client.list', {}).clients or {}) do
      if c.info and c.info.label == self.name and c.connection_state == 'connected' then
        for _, s in ipairs(c.session_ids or {}) do if s == session then return true end end
      end
    end
  end)
end

-- show attaches one more session; fail(code) changes the answer ('' answers {}).
function App:show(session) hub('hub.test.app', {name = self.name, sessions = {session}}); self:wait_shows(session) end
function App:fail(code) hub('hub.test.app', {name = self.name, fail = code or ''}) end
-- calls lists every client.perform that the app got: {action, args}.
function App:calls() return hub('hub.test.app.calls', {name = self.name}).calls end
function App:close() return hub('hub.test.app.close', {name = self.name}) end

-- restart_hub stops the hub and starts it again on the same lab server and state (hub:restart()
-- in a test file). crash = true kills it (SIGKILL); else it stops (SIGTERM). `lazyrex-hub test` does
-- the restart when it reads the request line; the runner waits until a new hub process is ready:
-- every plugin registered and the agent face lists its exports. Returns the new status.
-- The request goes out before any hub request: the hub can hold a call (a window hold), and then
-- Rex delivers it no other request until that call times out. The new hub is the connected
-- lazyrex-hub client with another client ID (a Rex client ID is not reused).
function H.restart_hub(opts)
  local old = hub_id
  io.write('LRT restart ', opts and opts.crash and 'kill' or 'term', '\n')
  io.flush()
  -- The wait returns the hub's ID and status together (no upvalue: a restart test once got nil).
  local ready = wait('the restarted hub', function()
    local id = resolve('lazyrex-hub')
    if not id or id == old then return nil end
    local st = perform(id, 'hub.test.status', {})
    if type(st) ~= 'table' or type(st.pid) ~= 'number' then return nil end
    for _, p in pairs(st.plugins or {}) do
      if not p.registered or #(p.face or {}) < #(p.exports or {}) then return nil end
    end
    return {id = id, status = st}
  end, 60)
  hub_id = ready.id
  return ready.status
end

function H.setup(args)
  A = args
  H.args = A
  rex.call('session.attach', {session_id = A.tests_session, event_scope = 'session'})
  local label = 'lazyrex-testrunner:' .. tostring(os.time()) .. ':' .. tostring(math.random(1, 1e9))
  rex.call('client.set_label', {label = label})
  self_id = wait('the runner client ID', function() return resolve(label) end)
  hub_id = wait('the hub', function() return resolve('lazyrex-hub') end)
  H.self_id = self_id
end

return H

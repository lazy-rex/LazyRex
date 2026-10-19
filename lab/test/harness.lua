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
  local payload = {client_id = client, action = action, args = args or {}, timeout_ms = timeout_ms or 10000}
  measure('client.perform', payload)
  local res, err = rex.call('client.perform', payload)
  if err ~= nil or res == nil then error(err_of(err or 'no result'), 0) end
  return res.result
end
H.perform = perform

local function resolve(label)
  local res, err = rex.call('target.resolve', {kinds = {'client'}, queries = {label}})
  if err or not res or not res.results or not res.results[1] then return nil end
  local r = res.results[1]
  local cands = r.status == 'resolved' and {r.target} or (r.candidates or {})
  local connected = {}
  for _, c in ipairs(cands) do
    local ins = rex.call('client.inspect', {client_id = c.client_id})
    if ins and ins.client and ins.client.connection_state == 'connected' then connected[#connected + 1] = c.client_id end
  end
  if #connected == 1 then return connected[1] end
  return nil
end
H.resolve = resolve

local function hub(action, args) return perform(hub_id, action, args) end
H.hub = hub

-- wait polls fn until it returns a value other than nil or false. It raises after timeout seconds
-- with what, and with the last error fn raised.
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
  local parts = {}
  if is_array(v) then
    for _, x in ipairs(v) do parts[#parts + 1] = show(x, depth + 1) end
  else
    local keys = {}
    for k in pairs(v) do keys[#keys + 1] = k end
    table.sort(keys, function(a, b) return tostring(a) < tostring(b) end)
    for _, k in ipairs(keys) do parts[#parts + 1] = tostring(k) .. '=' .. show(v[k], depth + 1) end
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

local function what(e) return e.label and (e.label .. ': ') or '' end

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
function H.inject(spec)
  local a = {plugin = spec.plugin or A.plugin, match = spec.match, ops = spec.ops, replay = spec.replay, times = spec.times,
    fail = spec.fail}
  return hub('hub.test.inject', a).id
end
function H.injection(id) return hub('hub.test.injection', {id = id}) end

-- with_injection runs fn with an injection and checks that exactly n batches consumed it.
function H.with_injection(spec, n, fn)
  local id = H.inject(spec)
  local r = table.pack and table.pack(fn()) or {fn()}
  local got = H.injection(id).consumed
  if got ~= n then fail(string.format('injection %d: expected %d matching batches, got %d', id, n, got), 2) end
  return (unpack or table.unpack)(r)
end

-- write commits store ops in the plugin's namespace now, as another writer: {key, value} or
-- {key, delete = true}.
function H.write(ops, plugin) return hub('hub.test.write', {plugin = plugin or A.plugin, ops = ops}) end

-- put_operation saves an operation record {key, fingerprint, result}, as an older plugin saved it.
function H.put_operation(op, plugin)
  return hub('hub.test.op.put', {plugin = plugin or A.plugin, key = op.key, fingerprint = op.fingerprint, result = op.result})
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
  local _, err = rex.call('session.new_window', {session_id = sess, window_label = opts.title or id,
    layout = {block = {flavor = 'com.superlogical.terminal.shell', options = {command = cmd}}}})
  if err then error(err_of(err), 0) end
  local a = setmetatable({id = id, label = 'lazyrex-testagent:' .. id, session = sess}, Agent)
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

-- call runs an exported plugin action through the agent face: the MCP path.
function Agent:call(action, args) return perform(self.client, 'agent.call', {action = action, args = args or {}}) end
-- direct calls the plugin client itself (not through the hub).
function Agent:direct(action, args, plugin)
  return perform(self.client, 'agent.direct', {plugin = plugin or A.plugin, action = action, args = args or {}})
end
function Agent:hub(action, args) return perform(self.client, 'agent.hub', {action = action, args = args or {}}) end
-- report sends hub.identity.report from a new process in this agent's pane, like an agent hook.
function Agent:report(r)
  return perform(self.client, 'agent.report', {args = {kind = r.kind, conversation_id = r.conversation_id, source = r.source or 'startup'}}, 20000)
end

-- --- The UI user (testview renderer panes) ---

local View = {}
View.__index = View
local User = {}
User.__index = User
H.user = setmetatable({}, User)

local function view_of(info)
  local v = setmetatable({id = info.view_id, route = info.route, tab = info.tab, plugin = info.plugin or A.plugin,
    label = 'lazyrex-testview:' .. info.view_id, context = info.context}, View)
  v.client = wait('the renderer of ' .. v.route .. ' to attach', function()
    for _, w in ipairs(H.views(v.plugin)) do
      if w.view_id == v.id and w.attached then return resolve(v.label) end
    end
    return nil
  end, 15)
  return v
end

-- open navigates to a tab of the plugin, as the user does, and returns its view.
function User:open(tab, plugin)
  plugin = plugin or A.plugin
  hub('hub.ui.navigate', {plugin = plugin, tab = tab})
  return self:view(tab, plugin)
end

-- view returns the open view of a tab key or a route (a form route, for example).
function User:view(key, plugin)
  plugin = plugin or A.plugin
  local info = wait('an open view of ' .. key, function()
    for _, w in ipairs(H.views(plugin)) do
      if w.tab == key or w.route == key or w.route == '/' .. key then w.plugin = plugin; return w end
    end
    return nil
  end, 15)
  return view_of(info)
end

-- command runs a palette command (plugin.id) from a pane, as the Rex palette does.
function User:command(id, from_block, plugin)
  plugin = plugin or A.plugin
  local blocks = rex.call('session.list_blocks', {session_id = A.tests_session})
  from_block = from_block or (blocks and blocks.blocks and blocks.blocks[1] and blocks.blocks[1].block_id)
  return hub('hub.palette.run', {command = plugin .. '.' .. id, ctx = {client_id = self_id, session_id = A.tests_session, block_id = from_block}})
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
function View:status() return perform(self.client, 'testview.status', {}) end

local op_seq = 0
local function new_key(prefix)
  op_seq = op_seq + 1
  return (prefix or 'op') .. '-' .. os.time() .. '-' .. op_seq
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

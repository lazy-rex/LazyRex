-- Tests of the SDK (lazyrex.lua) against the fake hub.sql. The to-do example (examples/todo) is the
-- plugin under test, unless a test gives its own main source.
local sandbox = dofile(tests_dir .. '/sandbox.lua')
local TODO = sdk_dir .. '/examples/todo/main.lua'
local base = getfenv(1)

local function todo(opts)
  opts = opts or {}
  opts.main = opts.main or (not opts.source and TODO) or nil
  local s = sandbox(sdk_dir, base, opts)
  if not s.loaded then error('the plugin loads: ' .. tostring(s.load_error), 2) end
  s:start()
  return s
end
local function find(list, pred) for _, x in ipairs(list or {}) do if pred(x) then return x end end end
local function action_names(s) local out = {}; for _, c in ipairs(s.sql.calls) do out[#out + 1] = c.action end; return table.concat(out, ' ') end
local function plugin(source, opts) opts = opts or {}; opts.source = 'local app = require("lazyrex").plugin("p")\n' .. source; return todo(opts) end

-- Registration and migrations ------------------------------------------------------------------

test('the start migrates in one transaction, then registers the actions, the exports and the panel', function()
  local s = todo()
  expect(action_names(s)).to_be('hub.sql.begin hub.sql.query hub.sql.exec hub.sql.exec hub.sql.commit')
  expect(s.sql.state.user_version).to_be(1)
  expect(s.sql.state.tables.todos ~= nil, 'the table').to_be(true)
  expect(s.registered.plugin).to_be('todo')
  expect(s.registered.instance_id).to_be('todo-1')
  expect(s.registered.exports).to_equal({'todo.add', 'todo.toggle'})
  expect(s.registered.commands[1].route).to_be('/panels/todos')
  expect(s.registered.commands[1].surface).to_be('panel')
  local add = find(s.keymap, function(a) return a.name == 'todo.add' end)
  expect(add.args.required).to_equal({'text'})
  expect(add.args.additionalProperties).to_be(false)
  expect(add.args.properties.key.type, 'the SDK adds the optional key').to_be('string')
  expect(add.description).to_be('Add an item to the to-do list.')
  expect(find(s.keymap, function(a) return a.name == 'hub.forward' end) ~= nil, 'hub.forward').to_be(true)
  expect(s.label).to_be('lazyrex-plugin:todo')
end)

test('a second start runs no migration again', function()
  local s = todo()
  local again = sandbox(sdk_dir, base, {main = TODO})
  again.sql.state = s.sql.state
  again:start()
  expect(action_names(again)).to_be('hub.sql.begin hub.sql.query hub.sql.commit')
  expect(again.registered ~= nil, 'registered').to_be(true)
end)

test('a database with a newer schema stops the start', function()
  local s = sandbox(sdk_dir, base, {main = TODO})
  s.sql.state.user_version = 2
  local ok, err = pcall(function() s:start() end)
  expect(ok, tostring(err)).to_be(true)
  expect(s.exited).to_be(1)
  expect(s.registered ~= nil, 'registered first: the hub refuses SQL before it').to_be(true)
  expect(s.printed[1]).to_match('schema version 2')
end)

-- Agent actions ----------------------------------------------------------------------------------

test('an agent action runs in one transaction and saves its reply with the key', function()
  local s = todo()
  local before = #s.sql.calls
  local r = s:agent('todo.add', {text = 'buy milk', key = 'k1'})
  expect(r.result).to_equal({id = 1})
  expect(s.sql.rows('todos')[1].text).to_be('buy milk')
  local calls = {}
  for i = before + 1, #s.sql.calls do calls[#calls + 1] = s.sql.calls[i].action end
  expect(table.concat(calls, ' ')).to_be('hub.sql.replay hub.sql.begin hub.sql.exec hub.sql.commit')
  local commit = s.sql.calls[#s.sql.calls].args
  expect(commit.key).to_be('a/5:alice2:k1')
  expect(commit.reply).to_equal({id = 1})
  expect(commit.fingerprint).to_match('^v1/')
end)

test('a retry with the same key gets the first reply; the handler does not run again', function()
  local s = todo()
  s:agent('todo.add', {text = 'buy milk', key = 'k1'})
  local r = s:agent('todo.add', {text = 'buy milk', key = 'k1'})
  expect(r.result).to_equal({id = 1})
  expect(#s.sql.rows('todos')).to_be(1)
end)

test('the same key with other arguments is an operation conflict', function()
  local s = todo()
  s:agent('todo.add', {text = 'buy milk', key = 'k1'})
  local r = s:agent('todo.add', {text = 'buy eggs', key = 'k1'})
  expect(r.error.code).to_be('operation_conflict')
  expect(#s.sql.rows('todos')).to_be(1)
end)

test('keys of two agents do not meet', function()
  local s = todo()
  s:agent('todo.add', {text = 'a', key = 'k1'}, 7)
  local r = s:agent('todo.add', {text = 'b', key = 'k1'}, 8)
  expect(r.result).to_equal({id = 2})
end)

test('a call without a key runs each time and saves no reply', function()
  local s = todo()
  s:agent('todo.add', {text = 'a'})
  s:agent('todo.add', {text = 'a'})
  expect(#s.sql.rows('todos')).to_be(2)
  expect(next(s.sql.ops)).to_be(nil)
  expect(s.sql.count('hub.sql.replay')).to_be(0)
end)

test('the arguments are checked: missing, unknown, wrong type, too short', function()
  local s = todo()
  expect(s:agent('todo.add', {}).error.message).to_be('Missing argument text')
  expect(s:agent('todo.add', {text = 'a', color = 'red'}).error.message).to_be('Unknown argument color')
  expect(s:agent('todo.add', {text = 5}).error.message).to_be('Argument text must be string')
  expect(s:agent('todo.add', {text = ''}).error.code).to_be('invalid_input')
  expect(s:agent('todo.toggle', {id = 1.5}).error.message).to_be('Argument id must be integer')
  expect(#s.sql.rows('todos')).to_be(0)
end)

test('an unknown agent, a forward from a peer, and a direct call from a peer', function()
  local s = todo()
  expect(s:agent('todo.add', {text = 'a'}, 99).error.code, 'unknown pid').to_be('refused')
  expect(s:call('hub.forward', {action = 'todo.add', args = {text = 'a'}, requester_pid = 7}, 7).error.code, 'forward from a peer').to_be('refused')
  local r = s:call('todo.add', {text = 'direct'}, 7)
  expect(r.result).to_equal({id = 1})
  expect(s:agent('todo.missing', {}).error.code).to_be('refused')
end)

-- Errors and rollback ----------------------------------------------------------------------------

test("error('text') gives code failed and the text; the writes roll back", function()
  local s = plugin([[
app.db.migrate {'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)'}
app.action('p.bad', {}, function() app.db.exec('INSERT INTO todos (text) VALUES (?)', 'x'); error('it broke') end)
app.action('p.coded', {}, function() app.fail('conflict', 'Taken') end)
app.action('p.table', {}, function() error({code = 'not_found', message = 'Gone'}) end)]])
  local r = s:agent('p.bad', {key = 'k'})
  expect(r.error).to_equal({code = 'failed', message = 'it broke'})
  expect(#s.sql.rows('todos'), 'rolled back').to_be(0)
  expect(s.sql.open, 'no open transaction').to_be(nil)
  expect(s.sql.rollbacks).to_be(1)
  expect(next(s.sql.ops), 'no saved reply').to_be(nil)
  expect(s:agent('p.coded', {}).error).to_equal({code = 'conflict', message = 'Taken'})
  expect(s:agent('p.table', {}).error).to_equal({code = 'not_found', message = 'Gone'})
end)

test('a SQL error keeps its code, and the earlier writes of the handler roll back', function()
  local s = plugin([[
app.db.migrate {'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)'}
app.action('p.two', {}, function() app.db.exec('INSERT INTO todos (text) VALUES (?)', 'x'); app.db.exec('INSERT INTO todos (text) VALUES (?)', nil) end)]])
  local r = s:agent('p.two', {})
  expect(r.error.code).to_be('sql_error')
  expect(r.error.message).to_match('NOT NULL')
  expect(#s.sql.rows('todos')).to_be(0)
end)

test('a failed commit gives its code; nothing is committed and nothing is saved', function()
  local s = todo()
  s.sql.fail_next['hub.sql.commit'] = {code = 'tx_expired', message = 'The transaction expired'}
  local r = s:agent('todo.add', {text = 'a', key = 'k'})
  expect(r.error).to_equal({code = 'tx_expired', message = 'The transaction expired'})
  expect(#s.sql.rows('todos')).to_be(0)
  expect(next(s.sql.ops)).to_be(nil)
end)

test('a hub error text keeps the hub code; a lost call is unavailable', function()
  local sdk = sandbox(sdk_dir, base, {source = 'return 1'}).sdk
  local function code(text) local _, e = pcall(sdk.rex_error, text); return e.code, e.message end
  expect(code('client.perform failed: tx_expired: gone (status=500 code=internal)')).to_be('tx_expired')
  expect(select(2, code('client.perform failed: tx_expired: gone (status=500 code=internal)'))).to_be('gone')
  expect(code('client.perform failed: Nope (status=404 code=not_found)')).to_be('not_found')
  expect(code('client.perform request timed out after 12s')).to_be('unavailable')
  expect(code('x failed: y (status=503 code=unavailable)')).to_be('unavailable')
  expect(code('x failed: y (status=500 code=disconnected)')).to_be('unavailable')
end)

test('a request over 64 KiB fails with too_large before it is sent', function()
  local s = todo()
  local before = #s.sql.calls
  local r = s:agent('todo.add', {text = string.rep('x', 70000)})
  expect(r.error.code).to_be('too_large')
  local sent = {}
  for i = before + 1, #s.sql.calls do sent[#sent + 1] = s.sql.calls[i].action end
  expect(table.concat(sent, ' '), 'no exec reached the hub').to_be('hub.sql.begin hub.sql.rollback')
end)

-- Values ------------------------------------------------------------------------------------------

test('NULL, int and blob values in rows; nil parameters encode as tags', function()
  local got
  local s = plugin([[
app.action('p.read', {}, function()
  local rows = app.db.query('SELECT a, b, c FROM t', 1, nil, 'x', nil)
  return {b_is_nil = rows[1].b == nil, a = rows[1].a, c = rows[1].c, big = rows[2].a, bytes = rows[2].b, more = select(2, app.db.query('SELECT a FROM t'))}
end)]], {hub = {['hub.sql.query'] = function(a)
    got = got or a
    return {columns = {'a', 'b', 'c'}, rows = {{1, {null = true}, 3}, {{int = '9223372036854775807'}, {blob = 'AAEC/w=='}, {null = true}}}, complete = true}
  end}})
  local r = s:agent('p.read', {})
  expect(r.result.b_is_nil).to_be(true)
  expect(r.result.a).to_be(1)
  expect(r.result.c, 'a later column stays').to_be(3)
  expect(r.result.big).to_be('9223372036854775807')
  expect(r.result.bytes).to_be('\0\1\2\255')
  expect(r.result.more).to_be(false)
  expect(got.params).to_equal({1, {null = true}, 'x', {null = true}})
end)

test('an integer parameter above 2^53 is refused; lazyrex.int and lazyrex.blob are tags', function()
  -- A table field: after a caught error, a closure can write a detached copy of a local (rex-protocol.md, "Rex Lua runtime").
  local seen = {}
  local s = plugin([[
app.action('p.big', {}, function() app.db.exec('UPDATE t SET a = ? WHERE id = ?', 2^60, 1) end)
app.action('p.tags', {}, function() app.db.exec('UPDATE t SET a = ? WHERE id = ?', app.int('9007199254740993'), app.blob('\0\255')); return {} end)]],
    {hub = {['hub.sql.exec'] = function(a) seen.got = a; return {changes = 1, id = 0} end}})
  expect(s:agent('p.big', {}).error.code).to_be('invalid_input')
  expect(seen.got, 'not sent').to_be(nil)
  local tr = s:agent('p.tags', {})
  expect(tr.error, show(tr)).to_be(nil)
  expect(seen.got.params).to_equal({{int = '9007199254740993'}, {blob = 'AP8='}})
end)

test('base64 round trip of every byte value', function()
  local sdk = sandbox(sdk_dir, base, {source = 'return 1'}).sdk
  local all = {}
  for i = 0, 255 do all[#all + 1] = string.char(i) end
  local bytes = table.concat(all)
  for n = 0, 4 do expect(sdk.unbase64(sdk.base64(bytes:sub(1, #bytes - n))), 'length ' .. (256 - n)).to_be(bytes:sub(1, #bytes - n)) end
  expect(sdk.base64('foobar')).to_be('Zm9vYmFy')
  expect(sdk.base64('fo')).to_be('Zm8=')
end)

-- Panels ------------------------------------------------------------------------------------------

test('a panel render reads outside a transaction, passes the render ID, and drops functions', function()
  local s = todo()
  s:agent('todo.add', {text = 'buy milk'})
  local before = #s.sql.calls
  local r = s:render('/panels/todos', 'render-9')
  local tree = r.result.tree
  expect(tree.title).to_be('Todos')
  expect(tree.nodes[1].type).to_be('list')
  local item = tree.nodes[1].items[1]
  expect(item).to_equal({id = '1', label = 'buy milk', detail = '', action = {name = 'select', label = 'Select', enabled = true, tool = 'todo.toggle'}})
  local q = s.sql.calls[before + 1]
  expect(q.action).to_be('hub.sql.query')
  expect(q.args.render).to_be('render-9')
  expect(q.args.tx).to_be(nil)
  expect(#s.sql.calls, 'one call').to_be(before + 1)
  expect(rex.json.encode(r.result) ~= nil, 'the reply encodes').to_be(true)
end)

test('an empty list renders as a widget with the empty text', function()
  local s = todo()
  local tree = s:render('/panels/todos').result.tree
  expect(tree.nodes[1]).to_equal({id = 'todos', type = 'widget', text = 'No items. An agent adds one with todo.add.'})
end)

test('a select runs the panel again in the transaction and calls the item handler', function()
  local s = todo()
  s:agent('todo.add', {text = 'buy milk'})
  local before = #s.sql.calls
  local r = s:select('/panels/todos', 'todos', '1', 'op-1')
  expect(r.result).to_equal({id = 1})
  expect(s.sql.rows('todos')[1].done).to_be(1)
  local calls = {}
  for i = before + 1, #s.sql.calls do calls[#calls + 1] = s.sql.calls[i].action end
  expect(table.concat(calls, ' ')).to_be('hub.sql.replay hub.sql.begin hub.sql.query hub.sql.exec hub.sql.commit')
  expect(s.sql.calls[before + 3].args.tx, 'the panel read is in the transaction').to_be(s.sql.calls[before + 4].args.tx)
  expect(s.sql.calls[#s.sql.calls].args.key).to_be('u/4:op-1')
  -- The renderer sends the same operation again: it runs once.
  s:select('/panels/todos', 'todos', '1', 'op-1')
  expect(s.sql.rows('todos')[1].done).to_be(1)
end)

test('a select of an item that is gone is a visible not_found error, with no write', function()
  local s = todo()
  local r = s:select('/panels/todos', 'todos', '42')
  expect(r.error.code).to_be('not_found')
  expect(r.error.message).to_match('gone')
  expect(s.sql.open).to_be(nil)
end)

test('view calls come only from the hub, for the user', function()
  local s = todo()
  expect(s:call('view.render', {route = '/panels/todos', user = 'user'}, 7).error.code).to_be('refused')
  expect(s:call('view.render', {route = '/panels/todos', user = 'mallory'}).error.code).to_be('refused')
  expect(s:call('view.render', {route = '/panels/nope', user = 'user'}).error.code).to_be('not_found')
end)

-- Transactions and saved replies -------------------------------------------------------------

test('tx = false: each SQL call alone; a saved terminal reply replays without the handler', function()
  local s = plugin([[
app.db.migrate {'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)'}
runs = 0
app.action('p.long', {}, function()
  runs = runs + 1
  app.db.exec('INSERT INTO todos (text) VALUES (?)', 'a')
  app.db.exec('INSERT INTO todos (text) VALUES (?)', 'b')
  return {done = true}
end, {tx = false})]])
  local before = #s.sql.calls
  expect(s:agent('p.long', {key = 'k'}).result).to_equal({done = true})
  local calls = {}
  for i = before + 1, #s.sql.calls do calls[#calls + 1] = s.sql.calls[i].action .. (s.sql.calls[i].args.tx and '+tx' or '') end
  expect(table.concat(calls, ' ')).to_be('hub.sql.replay hub.sql.exec hub.sql.exec hub.sql.begin hub.sql.commit+tx')
  expect(s:agent('p.long', {key = 'k'}).result).to_equal({done = true})
  expect(#s.sql.rows('todos'), 'the handler ran once').to_be(2)
end)

test('a running reply is never saved: a retry with the same key runs the handler', function()
  local s = plugin([[
runs = 0
app.action('p.step', {}, function() runs = runs + 1; if runs < 3 then return {status = 'running'} end return {status = 'done'} end, {tx = false})]])
  expect(s:agent('p.step', {key = 'k'}).result.status).to_be('running')
  expect(next(s.sql.ops), 'nothing saved').to_be(nil)
  expect(s:agent('p.step', {key = 'k'}).result.status).to_be('running')
  expect(s:agent('p.step', {key = 'k'}).result.status).to_be('done')
  expect(s:agent('p.step', {key = 'k'}).result.status, 'the done reply replays').to_be('done')
  expect(s.sql.count('hub.sql.commit')).to_be(1)
end)

test('store = false: the SDK saves nothing, and every call reaches the handler', function()
  local s = plugin([[
runs = 0
app.action('p.own', {}, function(ctx) runs = runs + 1; return {runs = runs, key = ctx.key} end, {store = false})]])
  expect(s:agent('p.own', {key = 'k'}).result).to_equal({runs = 1, key = 'k'})
  expect(s:agent('p.own', {key = 'k'}).result).to_equal({runs = 2, key = 'k'})
  expect(s.sql.count('hub.sql.replay')).to_be(0)
  expect(s.sql.count('hub.sql.commit')).to_be(0)
end)

test('store = false keeps key out of the schema; the handler reads ctx.key', function()
  local s = plugin([[app.action('p.own', {key = 'string'}, function(ctx, args) return {ctx = ctx.key, args = args.key} end, {store = false})]])
  local r = s:agent('p.own', {key = 'k'})
  expect(r.result).to_equal({ctx = 'k'})
end)

test('db.transaction in a tx = false handler commits its statements together', function()
  local s = plugin([[
app.db.migrate {'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)'}
app.action('p.phase', {}, function()
  app.db.transaction(function()
    app.db.exec('INSERT INTO todos (text) VALUES (?)', 'a')
    app.db.exec('INSERT INTO todos (text) VALUES (?)', nil)
  end)
end, {tx = false})]])
  expect(s:agent('p.phase', {}).error.code).to_be('sql_error')
  expect(#s.sql.rows('todos'), 'all or nothing').to_be(0)
end)

test('db.batch runs its statements all or nothing', function()
  local s = plugin([[
app.db.migrate {'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)'}
app.action('p.batch', {}, function()
  local r = app.db.batch {{'INSERT INTO todos (text) VALUES (?)', 'a'}, {'INSERT INTO todos (text) VALUES (?)', 'b'}}
  return {ids = {r[1].id, r[2].id}}
end, {tx = false})]])
  expect(s:agent('p.batch', {}).result).to_equal({ids = {1, 2}})
end)

test('app.notice goes into the commit of the handler; a rollback has no notice', function()
  local s = plugin([[
app.action('p.wake', {fail = 'boolean?'}, function(ctx, args) app.notice('bob', true); if args.fail then error('no') end return {} end)]])
  s:agent('p.wake', {fail = true})
  expect(s.sql.notices, 'no notice after a rollback').to_be(nil)
  s:agent('p.wake', {})
  expect(s.sql.notices).to_equal({{agent = 'bob', pending = true}})
end)

test('app.on runs a Rex event in a transaction; an error is printed and the plugin goes on', function()
  local s = plugin([[
app.db.migrate {'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)'}
app.on('block_closed', function(ev)
  app.db.exec('INSERT INTO todos (text) VALUES (?)', ev.block_id)
  if ev.block_id == 'bad' then error('event broke') end
end)]])
  s.events.block_closed({}, {block_id = 'b1'})
  expect(s.sql.rows('todos')[1].text).to_be('b1')
  s.events.block_closed({}, {block_id = 'bad'})
  expect(#s.sql.rows('todos')).to_be(1)
  expect(s.printed[#s.printed]).to_match('event broke')
end)

test('app.view: a raw route prefix with render and view.action, and its operation key', function()
  local s = plugin([[
app.view('/forms/edit*', {
  render = function(ctx, args) return {title = 'Edit', nodes = {{id = 'f', type = 'form', fields = {{id = 'name', label = 'Name', kind = 'text', value = args.route}}, submit_action = {name = 'save', label = 'Save', enabled = true, user_only = 'navigation'}}}} end,
  ['view.action'] = function(ctx, args) return {close_form = true, key = ctx.key, values = ctx.input.values} end,
})]])
  local tree = s:call('view.render', {route = '/forms/edit?id=4', user = 'user'}).result.tree
  expect(tree.nodes[1].fields[1].value).to_be('/forms/edit?id=4')
  local r = s:call('view.action', {route = '/forms/edit?id=4', node_id = 'f', action = 'save', input = {values = {name = 'x'}}, user = 'user', operation_key = 'o9'})
  expect(r.result).to_equal({close_form = true, key = 'o9', values = {name = 'x'}})
end)

test('app.hub keeps the hub error code; app.args has the plugin args only', function()
  local s = plugin([[
app.action('p.raw', {}, function() return {args = app.args, ok = pcall(app.hub, 'hub.nothing', {}), err = select(2, pcall(app.hub, 'hub.nothing', {}))} end)]],
    {args = {token_file = '/x'}})
  local r = s:agent('p.raw', {}).result
  expect(r.args).to_equal({token_file = '/x'})
  expect(r.ok).to_be(false)
  expect(r.err.code).to_be('not_supported')
end)

test('fingerprints ignore key order and tell values apart', function()
  local sdk = sandbox(sdk_dir, base, {source = 'return 1'}).sdk
  expect(sdk.fingerprint({a = 1, b = 'x'})).to_be(sdk.fingerprint({b = 'x', a = 1}))
  expect(sdk.fingerprint({a = 1}) ~= sdk.fingerprint({a = '1'}), 'number and text').to_be(true)
  expect(sdk.fingerprint({a = 'b=c'}) ~= sdk.fingerprint({['a=b'] = 'c'}), 'framing').to_be(true)
end)

test('a key that is saved between the replay check and the commit gives the saved reply', function()
  local s = todo()
  -- Another call with the same key commits first: the commit answers replayed, and this
  -- handler's writes are not committed.
  s.sql.hooks['hub.sql.commit'] = function(a)
    if a.key then s.sql.ops[a.key] = {fingerprint = a.fingerprint, reply = {id = 99}} end
  end
  local r = s:agent('todo.add', {text = 'late', key = 'k1'})
  expect(r.result).to_equal({id = 99})
  expect(#s.sql.rows('todos')).to_be(0)
end)

test('ctx.aliases are the legacy IDs of the hub.identify reply', function()
  local s = plugin([[app.action('p.who', {}, function(ctx) return {agent = ctx.agent, aliases = ctx.aliases, session = ctx.session_id} end, {store = false})]],
    {legacy = {alice = {'claude', 'alice-old'}}})
  expect(s:agent('p.who', {}).result).to_equal({agent = 'alice', aliases = {'claude', 'alice-old'}, session = 'session-alice'})
  expect(s:agent('p.who', {}, 8).result.aliases, 'none').to_equal({})
end)

test('fingerprints tell an array from an object with text keys', function()
  local sdk = sandbox(sdk_dir, base, {source = 'return 1'}).sdk
  expect(sdk.fingerprint({value = {'x'}}) ~= sdk.fingerprint({value = {['1'] = 'x'}}), 'array and object').to_be(true)
  expect(sdk.fingerprint({value = {[2] = 'x'}}) ~= sdk.fingerprint({value = {['2'] = 'x'}}), 'number and text key').to_be(true)
end)

test('a user-only action keeps the UI operation key: a repeat runs once; an argument key cannot replace it', function()
  local s = plugin([[
runs = 0
app.action('p.mine', {}, function(ctx) runs = runs + 1; return {runs = runs, key = ctx.key} end, {user_only = true})]])
  local function ui(args) return s:call('p.mine', {user = 'user', operation_key = 'ui-key', args = args or {}}) end
  expect(ui().result).to_equal({runs = 1, key = 'ui-key'})
  expect(ui().result, 'the repeat replays').to_equal({runs = 1, key = 'ui-key'})
  expect(ui({key = 'other'}).error.code, 'an argument key').to_be('invalid_input')
  local entry = find(s.keymap, function(a) return a.name == 'p.mine' end)
  expect(entry.args.additionalProperties, 'the hub sends {user, operation_key, args}').to_be(true)
  expect(s.registered.exports, 'not exported').to_be(nil)
end)

test('a select of an item that is disabled now is refused before its handler runs', function()
  local s = plugin([[
app.db.migrate {'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)'}
app.panel('P', function()
  return {app.ui.list('l', {{id = 'a', label = 'A', enabled = false, on_select = function() app.db.exec('INSERT INTO todos (text) VALUES (?)', 'x') end}})}
end)]])
  local r = s:select('/panels/p', 'l', 'a')
  expect(r.error and r.error.code, show(r)).to_be('conflict')
  expect(#s.sql.rows('todos')).to_be(0)
end)

test('app.view with tx = false and store = false runs its handler with no transaction', function()
  local s = plugin([[
app.db.migrate {'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)'}
app.view('/forms/long', {['view.action'] = function(ctx) app.db.exec('INSERT INTO todos (text) VALUES (?)', 'step'); return {status = 'running'} end}, {tx = false, store = false})]])
  local starts = s.sql.count('hub.sql.begin')
  local r = s:call('view.action', {route = '/forms/long', node_id = 'f', action = 'go', input = {}, user = 'user', operation_key = 'o1'})
  expect(r.result).to_equal({status = 'running'})
  expect(s.sql.count('hub.sql.begin'), 'no begin after the start').to_be(starts)
  expect(s.sql.count('hub.sql.replay')).to_be(0)
  expect(#s.sql.rows('todos'), 'the exec ran alone').to_be(1)
end)

test('a full JSON Schema with a list of types accepts each of them', function()
  local s = plugin([[
app.action('p.any', {type = 'object', properties = {v = {type = {'string', 'integer'}}, n = {type = 'number'}}, required = {'v'}, additionalProperties = false},
  function(ctx, args) return {v = args.v} end, {store = false})]])
  expect(s:agent('p.any', {v = 'x'}).result).to_equal({v = 'x'})
  expect(s:agent('p.any', {v = 3}).result).to_equal({v = 3})
  expect(s:agent('p.any', {v = 3, n = 2}).result, 'an integer is a number').to_equal({v = 3})
  expect(s:agent('p.any', {v = true}).error.message).to_be('Argument v must be string or integer')
end)

test('a retry finds a reply that the agent saved under one of its legacy IDs', function()
  local s = todo({legacy = {alice = {'claude'}}})
  -- The call before the takeover: the agent was 'claude' then (no legacy IDs for it).
  s.agents[9] = 'claude'
  expect(s:agent('todo.add', {text = 'old', key = 'k1'}, 9).result).to_equal({id = 1})
  -- Now the same pane is alice, with the legacy ID claude: the same key replays.
  expect(s:agent('todo.add', {text = 'old', key = 'k1'}, 7).result).to_equal({id = 1})
  expect(#s.sql.rows('todos')).to_be(1)
  -- Other content under the same key is still a conflict.
  expect(s:agent('todo.add', {text = 'new', key = 'k1'}, 7).error.code).to_be('operation_conflict')
end)

test('the same operation key from another view is an operation conflict, as in the hub', function()
  local s = todo()
  s:agent('todo.add', {text = 'a'})
  local function sel(view) return s:call('view.action', {view_id = view, route = '/panels/todos', node_id = 'todos', action = 'select', input = {item_id = '1'}, user = 'user', operation_key = 'same'}) end
  expect(sel('view:1').result).to_equal({id = 1})
  expect(sel('view:1').result, 'a retry of the same view replays').to_equal({id = 1})
  expect(sel('view:2').error.code).to_be('operation_conflict')
  expect(s.sql.rows('todos')[1].done, 'the handler ran once').to_be(1)
end)

test('a Rex timeout code is unavailable (unknown outcome); a hub.sql timeout text stays timeout (rolled back)', function()
  local sdk = sandbox(sdk_dir, base, {source = 'return 1'}).sdk
  local function code(text) local _, e = pcall(sdk.rex_error, text); return e.code end
  expect(code('client.perform failed: no answer (status=504 code=timeout)')).to_be('unavailable')
  expect(code('client.perform failed: timeout: rolled back (status=500 code=internal)')).to_be('timeout')
end)

test('the keymap lists each view handler name of app.view, and the hub calls reach it', function()
  local s = plugin([[app.view('/thread/*', {render = function() return {title = 'T', nodes = {}} end, ['view.open'] = function(ctx, args) return {opened = args.route} end}, {tx = false, store = false})]])
  local entry = find(s.keymap, function(a) return a.name == 'view.open' end)
  expect(entry ~= nil, 'view.open in the keymap').to_be(true)
  expect(find(s.keymap, function(a) return a.name == 'render' end), 'render is not a hub action').to_be(nil)
  local count = 0
  for _, a in ipairs(s.keymap) do if a.name == 'view.render' then count = count + 1 end end
  expect(count, 'one view.render').to_be(1)
  expect(s:call('view.open', {route = '/thread/7', user = 'user', node_id = 'x', operation_key = 'o'}).result).to_equal({opened = '/thread/7'})
end)

test('app.start with after_register = true runs after hub.register', function()
  local s = plugin([[
order = {}
app.start(function() order[#order + 1] = 'before' end)
app.start(function() order[#order + 1] = 'after:' .. tostring(registered_seen) end, {after_register = true, tx = false})
app.action('p.order', {}, function() return {order = order} end, {store = false})]],
    {hub = {['hub.register'] = function(a) registered_seen = true; return {} end}})
  expect(s:agent('p.order', {}).result.order).to_equal({'before', 'after:true'})
end)

test('the SDK registers before its first SQL call (the hub refuses SQL from an unregistered instance)', function()
  local seen = {}
  local s = sandbox(sdk_dir, base, {main = TODO, hub = {['hub.sql.begin'] = function(a)
    if not seen.registered then return nil, {code = 'refused', message = 'only a registered plugin instance may call hub.sql.begin'} end
    seen.begins = (seen.begins or 0) + 1
    return {tx = 'tx-r'}
  end, ['hub.register'] = function() seen.registered = true; return {} end,
  ['hub.sql.commit'] = function() return {} end, ['hub.sql.query'] = function() return {columns = {'user_version'}, rows = {{0}}, complete = true} end,
  ['hub.sql.exec'] = function() return {changes = 0, id = 0} end}})
  s:start()
  expect(s.exited, show(s.printed)).to_be(nil)
  expect(seen.begins, 'the migration ran').to_be(1)
end)

test('a plugin with no agent actions declares no hub.forward', function()
  local s = plugin([[app.action('p.mine', {}, function() return {} end, {user_only = true})]])
  expect(find(s.keymap, function(a) return a.name == 'hub.forward' end), 'no hub.forward').to_be(nil)
  expect(s.registered.exports).to_be(nil)
end)

test('app.on gives the Rex event context as the second argument', function()
  local s = plugin([[
app.on('terminal.child_exited', function(ev, ctx) seen_block = ctx and ctx.block_id; seen_code = ev.exit_code end, {tx = false})
app.action('p.seen', {}, function() return {block = seen_block, code = seen_code} end, {store = false})]])
  s.events['terminal.child_exited']({block_id = 'b7', session_id = 's1'}, {exit_code = 3})
  expect(s:agent('p.seen', {}).result).to_equal({block = 'b7', code = 3})
end)

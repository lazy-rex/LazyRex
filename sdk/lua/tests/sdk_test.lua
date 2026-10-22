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

test('a database with a newer schema: the start runs on, keeps user_version and logs one line', function()
  -- Expand/contract (docs/sql.md section 1): a rollback runs the previous release on the newer schema.
  local s = sandbox(sdk_dir, base, {main = TODO})
  s.sql.state.user_version = 2
  s.sql.state.tables.todos = {columns = {'id', 'text', 'done', 'note'}, rows = {}, seq = 0} -- schema 2 added note
  local ok, err = pcall(function() s:start() end)
  expect(ok, tostring(err)).to_be(true)
  expect(s.exited, 'the plugin runs on').to_be(nil)
  expect(s.registered ~= nil, 'registered').to_be(true)
  expect(action_names(s), 'no migration and no user_version write').to_be('hub.sql.begin hub.sql.query hub.sql.commit')
  expect(s.sql.state.user_version, 'not lowered').to_be(2)
  expect(#s.printed).to_be(1)
  expect(s.printed[1]).to_be('lazyrex: the database has schema 2; this plugin knows 1; running on (expand-only rule, docs/sql.md)')
  expect(s:agent('todo.add', {text = 'buy milk'}).result).to_equal({id = 1})
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
  return {b_is_nil = rows[1].b == nil, a = rows[1].a, c = rows[1].c, big = rows[2].a, bytes = rows[2].b}
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

-- Forms ------------------------------------------------------------------------------------------

local FORM = [[
app.db.migrate {'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)'}
app.form('/forms/add', {
  title = function(ctx) return 'Add to ' .. (ctx.query.list or 'the list') end,
  intro = 'One item per line is not supported.',
  fields = function(ctx) return {
    {id = 'text', label = 'Text', kind = 'text', required = true, value = ''},
    {id = 'urgent', label = 'Urgent', kind = 'boolean', value = false},
    {id = 'where', label = 'Where', kind = 'select', value = 'top', options = {{value = 'top', label = 'Top'}, {value = 'end', label = 'End'}}},
  } end,
  submit = {label = 'Add', tool = 'todo.add'},
  on_submit = function(ctx, values)
    if values.text == 'bad' then app.fail('invalid_input', 'Text: not this one') end
    app.db.exec('INSERT INTO todos (text) VALUES (?)', values.text)
    if values.where == 'end' then return {form = app.route('/forms/add', {list = 'b & c'})} end
  end,
  command = {id = 'add', title = 'Add Item…', description = 'Add an item', keywords = {'todo'}},
})
app.action('todo.add', {text = 'string'}, function() return {} end)
app.form('/forms/broken', {title = 'Broken', fields = function() app.fail('not_found', 'This list is gone') end, on_submit = function() end})
runs = 0
app.form('/forms/long', {title = 'Long', fields = {}, submit = {user_only = 'navigation'}, tx = false, store = false, command = {id = 'long', title = 'Long'},
  on_submit = function() runs = runs + 1; if runs < 2 then return {status = 'running'} end return {close_form = true, runs = runs} end})]]

local function submit(s, route, values, key)
  return s:call('view.action', {route = route, node_id = 'form', action = 'submit', input = {values = values}, user = 'user', operation_key = key or 'f1', view_id = 'v1'})
end

test('app.form renders the title, the intro, the fields, the submit with its tool and Cancel', function()
  local s = plugin(FORM)
  local tree = s:call('view.render', {route = '/forms/add?list=Home%20list', user = 'user'}).result.tree
  expect(tree.title).to_be('Add to Home list')
  expect(tree.nodes[1]).to_equal({id = 'intro', type = 'widget', text = 'One item per line is not supported.'})
  expect(tree.nodes[2].type).to_be('form')
  expect(#tree.nodes[2].fields).to_be(3)
  expect(tree.nodes[2].submit_action).to_equal({name = 'submit', label = 'Add', enabled = true, tool = 'todo.add'})
  expect(tree.nodes[3].action.name).to_be('$cancel')
  expect(tree.status.kind).to_be('question')
  local cmd = find(s.registered.commands, function(c) return c.id == 'add' end)
  expect(cmd.route).to_be('/forms/add')
  expect(cmd.action, 'the command names the tool').to_be('todo.add')
  local long = find(s.registered.commands, function(c) return c.id == 'long' end)
  expect(long.user_only, 'a form without a tool: the command is user_only').to_be('navigation')
end)

test('a form submit checks the values, runs on_submit in one transaction and closes the form', function()
  local s = plugin(FORM)
  expect(submit(s, '/forms/add', {text = '', urgent = false, where = 'top'}).error.message).to_be('Fill in Text')
  expect(submit(s, '/forms/add', {text = 'x', where = 'middle'}).error.message).to_be('Where: choose one of the options')
  expect(submit(s, '/forms/add', {text = 'x', color = 'red'}).error.message).to_be('Unknown field color')
  expect(submit(s, '/forms/add', {text = 5}).error.message).to_be('Text must be string')
  expect(submit(s, '/forms/add', {text = 'bad'}, 'f0').error).to_equal({code = 'invalid_input', message = 'Text: not this one'})
  expect(#s.sql.rows('todos'), 'no write yet').to_be(0)
  expect(submit(s, '/forms/add', {text = 'milk', urgent = true, where = 'top'}).result).to_equal({close_form = true})
  expect(s.sql.rows('todos')[1].text).to_be('milk')
  expect(submit(s, '/forms/add', {text = 'milk', urgent = true, where = 'top'}).result, 'a repeat replays').to_equal({close_form = true})
  expect(#s.sql.rows('todos')).to_be(1)
  local r = submit(s, '/forms/add?list=a', {text = 'eggs', where = 'end'}, 'f2')
  expect(r.result.form).to_be('/forms/add?list=b%20%26%20c')
end)

test('a form whose fields fail shows the message in place of the form', function()
  local s = plugin(FORM)
  local tree = s:call('view.render', {route = '/forms/broken', user = 'user'}).result.tree
  expect(tree.nodes[1]).to_equal({id = 'error', type = 'error', text = 'This list is gone'})
  expect(tree.nodes[2].action.name).to_be('$cancel')
  expect(submit(s, '/forms/broken', {}).error.code).to_be('not_found')
end)

test('a long form operation returns running until it ends; running is never saved', function()
  local s = plugin(FORM)
  expect(submit(s, '/forms/long', {}, 'L').result).to_equal({status = 'running'})
  expect(submit(s, '/forms/long', {}, 'L').result).to_equal({close_form = true, runs = 2})
  expect(s.sql.count('hub.sql.begin') - 1, 'no transaction besides the migration').to_be(0)
end)

test('ctx.query decodes the route query; a malformed escape drops that pair', function()
  local s = plugin([[app.view('/forms/q', {render = function(ctx) return {title = rex.json.encode(ctx.query), nodes = {}} end}, {tx = false})]])
  local title = s:call('view.render', {route = '/forms/q?a=1%2B1&b=x+y&c=%zz&d=%4', user = 'user'}).result.tree.title
  expect(rex.json.decode(title)).to_equal({a = '1+1', b = 'x y'})
  expect(s.sdk.route('/p', {b = 'é', a = 'x/y'})).to_be('/p?a=x%2Fy&b=%C3%A9')
end)

test('app.form options: the form node id and label, intro widgets, and a required checkbox', function()
  local s = plugin([[
app.form('/forms/ok', {title = 'OK', id = 'confirm', label = 'Confirm it',
  intro = function(ctx) return {{id = 'what', text = 'What: ' .. (ctx.query.what or '?')}, {id = 'risk', text = 'A risk.'}} end,
  fields = {{id = 'accept', label = 'I accept', kind = 'boolean', required = true, value = false}},
  submit = {name = 'go', label = 'Go', user_only = 'navigation'},
  on_submit = function(ctx, values) return {accepted = values.accept, close_form = true} end, store = false})]])
  local tree = s:call('view.render', {route = '/forms/ok?what=x', user = 'user'}).result.tree
  expect(tree.nodes[1]).to_equal({id = 'what', type = 'widget', text = 'What: x'})
  expect(tree.nodes[2]).to_equal({id = 'risk', type = 'widget', text = 'A risk.'})
  expect(tree.nodes[3].id).to_be('confirm')
  expect(tree.nodes[3].label).to_be('Confirm it')
  local function go(values) return s:call('view.action', {route = '/forms/ok?what=x', node_id = 'confirm', action = 'go', input = {values = values}, user = 'user', operation_key = 'g'}) end
  expect(go({accept = false}).error.message).to_be('Check I accept')
  expect(go({accept = true}).result).to_equal({accepted = true, close_form = true})
end)

test('minLength and maxLength count characters, not UTF-8 bytes', function()
  local s = plugin([[app.action('p.title', {title = {type = 'string', minLength = 2, maxLength = 60}}, function(ctx, args) return {ok = true} end, {store = false})]])
  local e = '\195\169' -- é: two bytes, one character
  expect(s:agent('p.title', {title = string.rep(e, 60)}).result, '60 characters').to_equal({ok = true})
  expect(s:agent('p.title', {title = string.rep(e, 61)}).error.message).to_be('Argument title is too long')
  expect(s:agent('p.title', {title = e}).error.message, 'one character, two bytes').to_be('Argument title is too short')
  expect(s:agent('p.title', {title = '\240\159\152\128\240\159\152\128'}).result, 'two 4-byte characters').to_equal({ok = true})
end)

-- Reply parts (chunking v1.1 with amendments 1-3). The sandbox hub answers hub.register with the
-- limits inline_max 45056, part_bytes 57344, max_total 786432.
local function joined(s, stub)
  local p = stub.lazyrex_parts
  return table.concat(s.part_sets[p.id] or {})
end
local function check_parts(s, stub, what)
  local p = stub.lazyrex_parts
  local mine = {}
  for _, c in ipairs(s.parts) do if c.id == p.id then mine[#mine + 1] = c end end
  expect(#mine, what .. ': the part calls').to_be(p.count)
  for i, c in ipairs(mine) do
    expect(c.index, what .. ': index').to_be(i - 1)
    expect(c.count, what .. ': count').to_be(p.count)
    expect(#c.data > 0, what .. ': no empty part').to_be(true)
    local request = rex.json.encode({client_id = 'hub', action = 'hub.reply.part', args = c, timeout_ms = 10000})
    expect(#request <= 60 * 1024, what .. ': part ' .. c.index .. ' request has ' .. #request .. ' bytes').to_be(true)
    expect(#rex.json.encode(c.data) - 2 <= 57344, what .. ': escaped data of part ' .. c.index).to_be(true)
  end
  expect(#joined(s, stub), what .. ': bytes').to_be(p.bytes)
end

test('a render reply over inline_max goes as parts, then a stub; the parts join to the reply', function()
  local s = plugin([[
local big = string.rep('x', 150 * 1024)
app.panel('Big', function() return {app.ui.text('t', big)} end, {id = 'big'})
app.panel('Small', function() return {app.ui.text('t', 'small')} end, {id = 'small'})]])
  local r = s:render('/panels/big', 'render-1')
  expect(r.error).to_be(nil)
  local stub = r.result
  expect(type(stub.lazyrex_parts), show(r)).to_be('table')
  expect(stub.lazyrex_parts.id).to_match('^todo%-1%-%d+$')
  check_parts(s, stub, 'big')
  local whole = rex.json.decode(joined(s, stub))
  expect(whole.tree.nodes[1].text == string.rep('x', 150 * 1024), 'the joined tree').to_be(true)
  local n = #s.parts
  local small = s:render('/panels/small', 'render-2')
  expect(small.result.lazyrex_parts, 'a small reply is whole').to_be(nil)
  expect(#s.parts, 'no part for a small reply').to_be(n)
end)

test('inline_max is the boundary: at it the reply is whole; a 50000-byte reply is one part', function()
  local s = plugin([[
app.action('p.size', {n = {type = 'integer'}}, function(ctx, args) return {t = string.rep('x', args.n)} end, {tx = false, store = false})]])
  local over = 45056 - #rex.json.encode({t = ''})
  expect(s:agent('p.size', {n = over}).result.t ~= nil, 'at inline_max: whole').to_be(true)
  local r = s:agent('p.size', {n = over + 1})
  expect(r.result.lazyrex_parts ~= nil, 'one byte over: parts').to_be(true)
  local one = s:agent('p.size', {n = 50000 - #rex.json.encode({t = ''})})
  expect(one.result.lazyrex_parts.count, 'a 50000-byte reply').to_be(1)
  check_parts(s, one.result, '50000')
end)

test('a reply over max_total fails with too_large before any part; a view too', function()
  local s = plugin([[
app.action('p.huge', {}, function() return {t = string.rep('x', 786432)} end, {tx = false, store = false})
app.panel('Huge', function() return {app.ui.text('t', string.rep('x', 786432))} end, {id = 'huge'})]])
  local r = s:agent('p.huge', {})
  expect(r.error and r.error.code, show(r)).to_be('too_large')
  expect(r.error.message).to_match('the limit is 786432')
  local v = s:render('/panels/huge', 'render-1')
  expect(v.error and v.error.code, show(v)).to_be('too_large')
  expect(#s.parts, 'no part sent').to_be(0)
end)

test('worst-case slices: quotes, backslashes, < > &, U+2028, multibyte text; each request fits and the join is exact', function()
  local cases = {['"'] = '"', ['\\'] = '\\', ['<>&'] = '<>&', ['U+2028'] = '\226\128\168', ['é'] = '\195\169', ['mixed'] = 'a"\\<\226\128\169é\1'}
  for name, unit in pairs(cases) do
    -- Decimal escapes: in Rex Lua, string.format('%q') writes U+2028 as \u2028, which its parser reads
    -- as u2028 (docs/rex-protocol.md, "Rex Lua runtime").
    local lit = '"' .. unit:gsub('.', function(c) return '\\' .. c:byte() end) .. '"'
    local s = plugin("app.action('p.w', {}, function() return {t = string.rep(" .. lit .. ", math.floor(50000 / #" .. lit .. "))} end, {tx = false, store = false})")
    local r = s:agent('p.w', {})
    expect(r.error, name).to_be(nil)
    expect(r.result.lazyrex_parts ~= nil, name .. ': parts').to_be(true)
    check_parts(s, r.result, name)
    local whole = rex.json.decode(joined(s, r.result))
    expect(whole.t == string.rep(unit, math.floor(50000 / #unit)), name .. ': the join is exact').to_be(true)
  end
end)

test('a refused part makes the reply that error', function()
  local s = plugin([[
app.action('p.big', {}, function() return {t = string.rep('x', 100000)} end, {tx = false, store = false})]],
    {hub = {['hub.reply.part'] = function() return nil, {code = 'parts_invalid', message = 'the set is refused'} end}})
  local r = s:agent('p.big', {})
  expect(r.error and r.error.code, show(r)).to_be('parts_invalid')
end)

test('a reply to a caller that is not the hub stays whole', function()
  local s = todo()
  local r = s:call('todo.add', {text = string.rep('x', 50), key = 'k1'}, 7)
  expect(r.error).to_be(nil)
  local s2 = plugin([[
app.action('p.big', {}, function() return {t = string.rep('x', 100000)} end, {tx = false, store = false})]])
  local direct = s2:call('p.big', {}, 7)
  expect(direct.result.t ~= nil, 'a direct agent call').to_be(true)
  expect(#s2.parts, 'no part').to_be(0)
end)

test('a saved large reply: the commit names the set, the reply is its stub, one upload; a replay answers it whole', function()
  local s = plugin([[
app.db.migrate {'CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)'}
app.action('p.save', {}, function()
  app.db.exec('INSERT INTO t (v) VALUES (?)', 'one')
  return {t = string.rep('x', 100000)}
end)]])
  local r = s:agent('p.save', {key = 'k1'})
  expect(r.error).to_be(nil)
  local stub = r.result.lazyrex_parts
  expect(stub ~= nil, show(r)).to_be(true)
  local commit = find(s.sql.calls, function(c) return c.action == 'hub.sql.commit' and c.args.key ~= nil end)
  expect(commit.args.reply, 'no inline reply').to_be(nil)
  expect(commit.args.reply_parts).to_equal(stub)
  local sets = 0
  for _ in pairs(s.part_sets) do sets = sets + 1 end
  expect(sets, 'one upload').to_be(1)
  -- A retry with the same key: the saved reply is a new reply, sent in parts again; one row.
  local again = s:agent('p.save', {key = 'k1'})
  expect(again.result.lazyrex_parts ~= nil, 'the replay is in parts').to_be(true)
  expect(again.result.lazyrex_parts.id ~= stub.id, 'a new set').to_be(true)
  expect(rex.json.decode(joined(s, again.result)).t == string.rep('x', 100000), 'the saved reply whole').to_be(true)
  expect(#s.sql.state.tables.t.rows, 'one mutation').to_be(1)
end)

test('a plugin result with the one key lazyrex_parts fails with reserved_result before any send', function()
  local s = plugin([[
app.action('p.fake', {}, function() return {lazyrex_parts = {id = 'x', count = 1, bytes = 1}} end, {tx = false, store = false})
app.action('p.two', {}, function() return {lazyrex_parts = 1, other = 2} end, {tx = false, store = false})]])
  local r = s:agent('p.fake', {})
  expect(r.error and r.error.code, show(r)).to_be('reserved_result')
  expect(s:agent('p.two', {}).error, 'another key besides it is plugin data').to_be(nil)
  expect(#s.parts).to_be(0)
end)

test('a hub.register reply without limits fails the start', function()
  local s = sandbox(sdk_dir, base, {source = 'local app = require("lazyrex").plugin("p")', limits = false})
  s:start()
  expect(s.exited).to_be(1)
  expect(find(s.printed, function(l) return l:find('no limits', 1, true) ~= nil end) ~= nil, show(s.printed)).to_be(true)
end)

local FORM_PARTS = [[
app.db.migrate {'CREATE TABLE picks (id INTEGER PRIMARY KEY, what TEXT NOT NULL)'}
local function pick(what) return function() app.db.exec('INSERT INTO picks (what) VALUES (?)', what); return {form = app.route('/forms/parts', {page = what})} end end
app.form('/forms/parts', {title = 'Parts',
  intro = function(ctx) return {
    app.ui.text('note', 'Page ' .. (ctx.query.page or '1')),
    app.ui.widget('more', 'More profiles', pick('more'), {label = 'More', user_only = 'navigation'}),
  } end,
  after = function(ctx) return {app.ui.list('dirs', {
    {id = 'w1', label = 'Worktree 1', on_select = pick('w1'), tool = 'p.pick'},
    {id = 'w2', label = 'Worktree 2', on_select = pick('w2'), tool = 'p.pick', enabled = false},
  })} end,
  instead = function(ctx)
    if ctx.query.pending then return {nodes = {app.ui.widget('again', 'Pending', pick('again'), {label = 'Check again'})}, status = {state = 'working', msg = 'Starting'}} end
  end,
  fields = function(ctx) return {
    {id = 'set', label = 'Set', kind = 'select', value = 'k1', options = {{value = 'k1', label = 'k1'}}, check = false},
    {id = 'mode', label = 'Mode', kind = 'select', value = 'a', options = {{value = 'a', label = 'A'}}},
  } end,
  submit = {label = 'Go', user_only = 'navigation', enabled = function(ctx) return ctx.query.rev ~= 'old' end,
    refusal = function(ctx) return 'Rev ' .. ctx.query.rev .. ' is old. Open the form again.' end},
  on_submit = function(ctx, values) return {set = values.set, close_form = true} end, store = false})
app.action('p.pick', {}, function() return {} end)]]

local function form_click(s, route, node_id, item_id, key)
  return s:call('view.action', {route = route, node_id = node_id, action = 'select', input = {item_id = item_id}, user = 'user', operation_key = key or 'c1', view_id = 'v1'})
end

test('app.form: nodes with actions in intro and after; a click runs on_select in its transaction', function()
  local s = plugin(FORM_PARTS)
  local tree = s:call('view.render', {route = '/forms/parts', user = 'user'}).result.tree
  local ids = {}
  for _, n in ipairs(tree.nodes) do ids[#ids + 1] = n.id end
  expect(table.concat(ids, ' ')).to_be('note more form dirs cancel')
  expect(tree.nodes[2].action).to_equal({name = 'select', label = 'More', enabled = true, user_only = 'navigation'})
  expect(tree.nodes[4].items[1].action.tool).to_be('p.pick')
  expect(tree.nodes[4].items[2].action.enabled).to_be(false)
  expect(form_click(s, '/forms/parts', 'more').result).to_equal({form = '/forms/parts?page=more'})
  expect(form_click(s, '/forms/parts', 'dirs', 'w1', 'c2').result).to_equal({form = '/forms/parts?page=w1'})
  expect(form_click(s, '/forms/parts', 'dirs', 'w2', 'c3').error.code, 'a disabled item').to_be('conflict')
  expect(form_click(s, '/forms/parts', 'dirs', 'w9', 'c4').error.code, 'an item that is gone').to_be('not_found')
  expect(form_click(s, '/forms/parts', 'note', nil, 'c5').error.code, 'a widget with no action').to_be('not_found')
  local picks = {}
  for _, r in ipairs(s.sql.rows('picks')) do picks[#picks + 1] = r.what end
  expect(table.concat(picks, ' ')).to_be('more w1')
end)

test('app.form: instead replaces the form with another state; a submit then fails with conflict', function()
  local s = plugin(FORM_PARTS)
  local tree = s:call('view.render', {route = '/forms/parts?pending=1', user = 'user'}).result.tree
  local ids = {}
  for _, n in ipairs(tree.nodes) do ids[#ids + 1] = n.id end
  expect(table.concat(ids, ' ')).to_be('note more again dirs cancel')
  expect(tree.status).to_equal({state = 'working', msg = 'Starting'})
  expect(form_click(s, '/forms/parts?pending=1', 'again').result).to_equal({form = '/forms/parts?page=again'})
  local r = s:call('view.action', {route = '/forms/parts?pending=1', node_id = 'form', action = 'submit', input = {values = {set = 'k1', mode = 'a'}}, user = 'user', operation_key = 's1'})
  expect(r.error.code).to_be('conflict')
end)

test('app.form: submit.refusal gives the stale-submit message; check = false takes a select value that the render fixed', function()
  local s = plugin(FORM_PARTS)
  local function go(route, values) return s:call('view.action', {route = route, node_id = 'form', action = 'submit', input = {values = values}, user = 'user', operation_key = 's'}) end
  expect(go('/forms/parts?rev=old', {set = 'k1', mode = 'a'}).error).to_equal({code = 'conflict', message = 'Rev old is old. Open the form again.'})
  expect(go('/forms/parts', {set = 'k-shown-before', mode = 'a'}).result).to_equal({set = 'k-shown-before', close_form = true})
  expect(go('/forms/parts', {set = 'k1', mode = 'z'}).error.message, 'a checked select').to_be('Mode: choose one of the options')
  expect(go('/forms/parts', {set = 5, mode = 'a'}).error.message, 'the type is still checked').to_be('Set must be string')
end)

test('a plain form needs only title, fields and on_submit', function()
  local s = plugin([[
app.form('/forms/plain', {title = 'Plain', fields = {{id = 'text', label = 'Text', kind = 'text', required = true, value = ''}},
  on_submit = function() end})]])
  local tree = s:call('view.render', {route = '/forms/plain', user = 'user'}).result.tree
  local ids = {}
  for _, n in ipairs(tree.nodes) do ids[#ids + 1] = n.id end
  expect(table.concat(ids, ' ')).to_be('form cancel')
  expect(tree.nodes[1].submit_action).to_equal({name = 'submit', label = 'Submit', enabled = true, user_only = 'navigation'})
  expect(tree.status.msg).to_be('Waiting for input')
  local r = s:call('view.action', {route = '/forms/plain', node_id = 'form', action = 'submit', input = {values = {text = 'x'}}, user = 'user', operation_key = 'p1'})
  expect(r.result).to_equal({close_form = true})
end)

test('a 64 KB BLOB in a row decodes; a big blob parameter and a fingerprint of 5000 items work', function()
  -- Rex Lua: table.concat of more than about 2559 items fails with "registry overflow" (observed on
  -- Rex 0.0.1792158932-g88fd41, lab, 2026-10-22).
  local bytes = {}
  for i = 1, 65536 do bytes[i] = string.char(i % 256) end
  local big = {}
  for i = 1, 64 do big[i] = table.concat(bytes, '', (i - 1) * 1024 + 1, i * 1024) end
  big = table.concat(big)
  local sdk = sandbox(sdk_dir, base, {source = 'return 1'}).sdk
  local encoded = sdk.base64(big)
  local s = plugin([[
app.action('p.read', {}, function() local r = app.db.first('SELECT b FROM t'); return {n = #r.b, head = r.b:byte(1), tail = r.b:byte(-1)} end)]],
    {hub = {['hub.sql.query'] = function() return {columns = {'b'}, rows = {{{blob = encoded}}}, complete = true} end}})
  local r = s:agent('p.read', {})
  expect(r.error, show(r)).to_be(nil)
  expect(r.result).to_equal({n = 65536, head = 1, tail = 0})
  expect(sdk.unbase64(encoded) == big, 'round trip').to_be(true)
  local items = {}
  for i = 1, 5000 do items[i] = i end
  expect(sdk.fingerprint(items) ~= sdk.fingerprint({1}), 'a fingerprint of 5000 items').to_be(true)
end)

-- A fake hub.sql.query that holds 25 rows and answers at most 10 in one reply, as the hub cuts a
-- reply at its size limit (docs/sql.md section 4, paging). seen.calls logs {sql, params, render}.
local function paged_hub(seen)
  return {['hub.sql.query'] = function(a)
    seen.calls[#seen.calls + 1] = {sql = a.sql, params = a.params, render = a.render}
    local from = 0
    local wrapped = a.sql:match('^SELECT %* FROM %((.*)\n%) LIMIT %-1 OFFSET %?(%d+)$')
    if wrapped then from = a.params[tonumber(a.sql:match('%?(%d+)$'))] end
    if not wrapped and a.sql:find('OFFSET', 1, true) then error('the fake does not know this page query: ' .. a.sql) end
    local rows = {}
    for i = from + 1, math.min(from + 10, 25) do rows[#rows + 1] = {i} end
    return {columns = {'n'}, rows = rows, complete = from + 10 >= 25}
  end}
end

test('db.query fails with too_large when the result does not fit in one reply; db.each reads every row', function()
  local seen = {calls = {}}
  local s = plugin([[
app.action('p.query', {}, function() return {n = #app.db.query('SELECT n FROM t WHERE k = ?', 'x')} end)
app.action('p.first', {}, function() return {n = app.db.first('SELECT n FROM t WHERE k = ?', 'x').n} end)
app.action('p.each', {}, function()
  local got = {}
  local count = app.db.each('SELECT n FROM t WHERE k = ?;', 'x', function(row) got[#got + 1] = row.n end)
  return {count = count, first = got[1], last = got[#got], all = #got}
end)]], {hub = paged_hub(seen)})
  local r = s:agent('p.query', {})
  expect(r.error and r.error.code, show(r)).to_be('too_large')
  expect(r.error.message:find('app.db.each', 1, true) ~= nil, r.error.message).to_be(true)
  expect(s:agent('p.first', {}).result, 'first needs one row only').to_equal({n = 1})
  seen.calls = {}
  r = s:agent('p.each', {})
  expect(r.error, show(r)).to_be(nil)
  expect(r.result).to_equal({count = 25, first = 1, last = 25, all = 25})
  expect(#seen.calls).to_be(3)
  expect(seen.calls[2].sql).to_be('SELECT * FROM (SELECT n FROM t WHERE k = ?\n) LIMIT -1 OFFSET ?2')
  expect(seen.calls[2].params).to_equal({'x', 10})
  expect(seen.calls[3].params).to_equal({'x', 20})
end)

test('db.each: a line comment at the end of the SQL does not comment out the closing parenthesis', function()
  local seen = {calls = {}}
  local s = plugin([[
app.action('p.each', {}, function() return {count = app.db.each('SELECT n FROM t -- all of them', function() end)} end)]], {hub = paged_hub(seen)})
  local r = s:agent('p.each', {})
  expect(r.error, show(r)).to_be(nil)
  expect(r.result.count).to_be(25)
  expect(seen.calls[2].sql).to_be('SELECT * FROM (SELECT n FROM t -- all of them\n) LIMIT -1 OFFSET ?1')
end)

test('a render never opens a transaction: db.each pages without one; a write or db.transaction fails clearly', function()
  -- The hub sends no render token in view.render yet, so the frame must know that it is a render
  -- without one (hub lead, the render rule: a render reads the committed state, never in a transaction).
  local s = plugin([[
app.db.migrate {'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)'}
app.panel('Each', function()
  local items = {}
  app.db.each('SELECT id, text FROM todos', function(row) items[#items + 1] = {id = row.id, label = row.text} end)
  return {app.ui.list('todos', items)}
end, {id = 'each'})
app.panel('Write', function() app.db.exec('INSERT INTO todos (text) VALUES (?)', 'x'); return {} end, {id = 'write'})
app.panel('Tx', function() app.db.transaction(function() end); return {} end, {id = 'tx'})]])
  local before = s.sql.count('hub.sql.begin')
  local r = s:call('view.render', {route = '/panels/each', user = 'user'})
  expect(r.error, show(r)).to_be(nil)
  expect(s.sql.count('hub.sql.begin') - before, 'no hub.sql.begin in a render').to_be(0)
  local w = s:call('view.render', {route = '/panels/write', user = 'user'})
  expect(w.error and w.error.code, show(w)).to_be('refused')
  expect(w.error.message).to_be('A render cannot write: db.exec runs in an action or a select handler')
  expect(#s.sql.rows('todos'), 'nothing written').to_be(0)
  local t = s:call('view.render', {route = '/panels/tx', user = 'user'})
  expect(t.error and t.error.message).to_be('A render cannot write: db.transaction runs in an action or a select handler')
  expect(s.sql.count('hub.sql.begin') - before, 'still no hub.sql.begin').to_be(0)
end)

test('ctx.source has the pane directory that the hub froze at the open (form context source_pwd, source_pwd_error)', function()
  local s = plugin([[
local function show_source(ctx) return rex.json.encode({pwd = ctx.source.pwd or false, err = ctx.source.pwd_error or false}) end
app.form('/forms/src', {title = function(ctx) return show_source(ctx) end, fields = {},
  on_submit = function(ctx) return {source = show_source(ctx)} end, store = false})]])
  local function title(context)
    return rex.json.decode(s:call('view.render', {route = '/forms/src', user = 'user', view_id = 'v', context = context}).result.tree.title)
  end
  expect(title({source_block_id = 'b', source_pwd = 'file:///tmp/lazyrex%20pwd'})).to_equal({pwd = 'file:///tmp/lazyrex%20pwd', err = false})
  expect(title({source_block_id = 'b', source_pwd_error = 'the source pane reports no directory'})).to_equal({pwd = false, err = 'the source pane reports no directory'})
  expect(title({}), 'no source pane: neither field').to_equal({pwd = false, err = false})
  local r = s:call('view.action', {route = '/forms/src', node_id = 'form', action = 'submit', input = {values = {}}, user = 'user', operation_key = 'k',
    view_id = 'v', context = {source_pwd = 'file:///a'}})
  expect(rex.json.decode(r.result.source).pwd, 'an action gets it too').to_be('file:///a')
end)

test('a same-key retry gets its saved reply before the SDK checks the values or items against the new state', function()
  local s = plugin([[
app.db.migrate {'CREATE TABLE opts (id INTEGER PRIMARY KEY, name TEXT NOT NULL)'}
app.action('p.seed', {}, function() app.db.exec('INSERT INTO opts (name) VALUES (?)', 'a'); return {} end, {store = false})
app.action('p.clear', {}, function() app.db.exec('DELETE FROM opts WHERE name = ?', 'a'); return {} end, {store = false})
local function options() local out = {}; for _, r in ipairs(app.db.query('SELECT id, name FROM opts')) do out[#out + 1] = {value = r.name, label = r.name} end; return out end
app.form('/forms/pick', {title = 'Pick',
  fields = function() return {{id = 'name', label = 'Name', kind = 'select', value = '', options = options()}} end,
  on_submit = function(ctx, values) return {picked = values.name, close_form = true} end})
app.panel('Picks', function()
  local items = {}
  for _, o in ipairs(options()) do items[#items + 1] = {id = o.value, label = o.label, on_select = function() return {chose = o.value} end} end
  return {app.ui.list('picks', items)}
end, {id = 'picks'})]])
  s:agent('p.seed', {})
  local function pick() return s:call('view.action', {route = '/forms/pick', node_id = 'form', action = 'submit', input = {values = {name = 'a'}}, user = 'user', operation_key = 'k1', view_id = 'v'}) end
  local function choose() return s:call('view.action', {route = '/panels/picks', node_id = 'picks', action = 'select', input = {item_id = 'a'}, user = 'user', operation_key = 'k2', view_id = 'p'}) end
  expect(pick().result).to_equal({picked = 'a', close_form = true})
  expect(choose().result).to_equal({chose = 'a'})
  s:agent('p.clear', {})
  local r = pick()
  expect(r.error, show(r)).to_be(nil)
  expect(r.result, 'the form retry replays').to_equal({picked = 'a', close_form = true})
  r = choose()
  expect(r.error, show(r)).to_be(nil)
  expect(r.result, 'the list retry replays').to_equal({chose = 'a'})
end)

test('app.on hub.prompt: the hub calls it in a frame; an error reaches the hub; app.registration has the hub.register reply', function()
  local s = plugin([[
app.db.migrate {'CREATE TABLE prompts (id INTEGER PRIMARY KEY, agent TEXT NOT NULL, rev TEXT NOT NULL)'}
seen = {}
app.start(function() seen.generation = app.registration and app.registration.generation end, {after_register = true})
app.on('hub.prompt', function(args)
  if args.agent == 'bad' then error('cannot apply') end
  app.db.exec('INSERT INTO prompts (agent, rev) VALUES (?, ?)', args.agent, tostring(args.rev))
end)
app.action('p.seen', {}, function() return {generation = seen.generation, rows = #app.db.query('SELECT id FROM prompts')} end, {store = false})]],
    {hub = {['hub.register'] = function() return {generation = '42'} end}})
  expect(find(s.keymap, function(k) return k.name == 'hub.prompt' end) ~= nil, 'the keymap lists hub.prompt').to_be(true)
  local r = s:call('hub.prompt', {hub_generation = '42', rev = 3, agent = 'alice', prompt = {null = true}})
  expect(r.error, show(r)).to_be(nil)
  expect(r.result).to_equal({})
  local bad = s:call('hub.prompt', {hub_generation = '42', rev = 4, agent = 'bad'})
  expect(bad.error and bad.error.message, 'the error reaches the hub').to_be('cannot apply')
  expect(s:call('hub.prompt', {agent = 'alice', rev = 5}, 7).error.code, 'only the hub calls it').to_be('refused')
  expect(s:agent('p.seen', {}).result).to_equal({generation = '42', rows = 1})
  local plain = plugin([[app.action('p.x', {}, function() return {} end)]])
  expect(find(plain.keymap, function(k) return k.name == 'hub.prompt' end), 'no listener, no keymap entry').to_be(nil)
end)

test('app.notice in a render fails clearly and opens no transaction (a render only reads)', function()
  local s = plugin([[app.panel('Note', function() app.notice('alice', true); return {} end, {id = 'note'})]])
  local before = s.sql.count('hub.sql.begin')
  local r = s:call('view.render', {route = '/panels/note', user = 'user'})
  expect(r.error and r.error.message, show(r)).to_be('A render cannot write: app.notice runs in an action or a select handler')
  expect(s.sql.count('hub.sql.begin') - before, 'no hub.sql.begin').to_be(0)
end)

test('the example manifest and the docs manifest name the SDK, so the launcher runs boot.lua', function()
  -- The hub launcher runs a plugin through sdk/lua/boot.lua only when lazyrex.json has "sdk" (hub
  -- plugins.go, the SDK launch; reviewer codex-15).
  local function read(path) local f = assert(io.open(path)); local t = f:read('*a'); f:close(); return t end
  expect(rex.json.decode(read(sdk_dir .. '/examples/todo/lazyrex.json')).sdk).to_be('0')
  local doc = read(sdk_dir .. '/../../docs/plugin-sdk.md')
  local manifest = doc:match('`lazyrex.json`:%s*```json%s*(%b{})')
  expect(manifest ~= nil, 'the docs show a manifest').to_be(true)
  expect(rex.json.decode(manifest).sdk).to_be('0')
end)

test('notice v2: app.notice with once, and app.notice_read, go into the commit of the handler', function()
  local s = plugin([[
app.action('p.once', {}, function() app.notice('bob', true, {once = true}); return {} end)
app.action('p.read', {}, function() app.notice_read('bob'); return {} end)
app.action('p.bad', {}, function() app.notice('bob', false, {once = true}); return {} end)]])
  s:agent('p.once', {})
  expect(s.sql.notices).to_equal({{agent = 'bob', pending = true, once = true}})
  s.sql.notices = nil -- the fake keeps every committed notice
  s:agent('p.read', {})
  expect(s.sql.notices, 'read: no pending, no once').to_equal({{agent = 'bob', read = true}})
  local r = s:agent('p.bad', {})
  expect(r.error and r.error.message).to_be('app.notice: once needs pending = true')
end)

test('hub.sql changes on hub/sql-lab c5d228b: an exact insert ID; a statement error after which SQLite ended the transaction', function()
  -- 1. An exec or batch reply id has the integer form of a cell: {int = "<decimal>"} beyond 2^53.
  -- 2. After such an error every call with the token gives tx_expired, the rollback too. The handler
  --    must fail with the statement error, not with the rollback's tx_expired.
  local calls = {}
  local s = plugin([[
app.action('p.big', {}, function() return {id = app.db.exec('INSERT INTO t (a) VALUES (?)', 1).id, batch = app.db.batch({{'INSERT INTO t (a) VALUES (?)', 2}})[1].id} end)
app.action('p.boom', {}, function() app.db.exec('INSERT OR ROLLBACK INTO t (a) VALUES (?)', 1); return {} end)]],
    {hub = {
      ['hub.sql.exec'] = function(a)
        if a.sql:find('OR ROLLBACK', 1, true) then return nil, {code = 'sql_error', message = 'UNIQUE constraint failed: t.a; SQLite rolled back the transaction'} end
        return {changes = 1, id = {int = '9007199254740993'}}
      end,
      ['hub.sql.batch'] = function() return {results = {{changes = 1, id = {int = '9007199254740995'}}}} end,
      ['hub.sql.rollback'] = function() calls[#calls + 1] = 'rollback'; return nil, {code = 'tx_expired', message = 'the transaction ended'} end,
    }})
  local r = s:agent('p.big', {})
  expect(r.error, show(r)).to_be(nil)
  expect(r.result).to_equal({id = '9007199254740993', batch = '9007199254740995'})
  r = s:agent('p.boom', {})
  expect(r.error and r.error.code, show(r)).to_be('sql_error')
  expect(r.error.message:find('SQLite rolled back the transaction', 1, true) ~= nil, r.error.message).to_be(true)
  expect(calls[1], 'the SDK tried the rollback; its tx_expired is not the error').to_be('rollback')
end)

test('a panel with a title of several words gets a command id that the hub accepts (a-z, 0-9 and _)', function()
  -- The hub refuses another command id: 'command id "notes.add_note": use a-z, 0-9 and _'
  -- (observed by the fresh-agent doc test on hub/sql-r2 c840c92, 2026-10-22).
  local s = plugin([[app.panel('My Notes!', function() return {} end)]])
  local c = s.registered.commands[1]
  expect(c.id).to_be('my_notes')
  expect(c.route).to_be('/panels/my_notes')
  expect(c.id:match('^[a-z0-9_]+$') ~= nil, c.id).to_be(true)
  local bad = sandbox(sdk_dir, base, {source = 'local app = require("lazyrex").plugin("p")\napp.command({id = "notes.add", title = "Add", route = "/x"})'})
  expect(bad.loaded, 'a bad command id fails at the declaration').to_be(false)
  expect(tostring(bad.load_error):find('A command id uses only a-z, 0-9 and _ (not notes.add)', 1, true) ~= nil, tostring(bad.load_error)).to_be(true)
end)

test('a render is tracked by default; refresh = "all" and ctx.refresh_all() opt out; another value fails at load', function()
  -- Read tracking is the default (user decision, 2026-10-22): a tracked render sends no refresh field.
  local s = plugin([[
app.panel('Plain', function() return {} end)
app.panel('All', function() return {} end, {refresh = 'all'})
app.view('/v/*', {render = function(ctx)
  if ctx.route == '/v/all' then ctx.refresh_all() end
  return {title = 'V', nodes = {}}
end})
app.view('/w', {render = function() return {title = 'W', nodes = {}} end}, {refresh = 'all'})
]])
  for _, c in ipairs({{'/panels/plain', nil}, {'/panels/all', 'all'}, {'/v/one', nil}, {'/v/all', 'all'}, {'/v/one', nil}, {'/w', 'all'}}) do
    local r = s:render(c[1], 'render-1')
    expect(r.error, c[1]).to_be(nil)
    expect(r.result.refresh, c[1]).to_be(c[2])
    expect(r.result.tree.refresh, c[1] .. ': not in the tree').to_be(nil)
  end
  for _, decl in ipairs({"app.panel('P', function() return {} end, {refresh = 'reads'})",
      "app.panel('P', function() return {} end, {refresh = 'writes'})",
      "app.view('/v', {render = function() return {} end}, {refresh = true})"}) do
    local bad = sandbox(sdk_dir, base, {source = 'local app = require("lazyrex").plugin("p")\n' .. decl})
    expect(bad.loaded, decl .. ': fails at the declaration').to_be(false)
    expect(tostring(bad.load_error):find('refresh is "all" or absent', 1, true) ~= nil, tostring(bad.load_error)).to_be(true)
  end
end)

test('a failed hub call in a render replies refresh = "all", also when the plugin catches the error', function()
  -- The hub tracks a render only when every hub call of the render reached the hub and returned
  -- without error (hub lead, lead-render-incomplete). Only the SDK sees a call that never reached it.
  local s = plugin([[
app.db.migrate {'CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)'}
app.view('/v/*', {render = function(ctx)
  local ok
  if ctx.route == '/v/raw' then ok = pcall(app.hub, 'hub.sql.query', {sql = 'SELECT id FROM t', params = {}})
  else ok = pcall(app.db.first, 'SELECT id FROM t') end
  return {title = ok and 'read' or 'caught', nodes = {}}
end})
app.panel('Enc', function()
  pcall(app.hub, 'hub.sql.query', {sql = 'SELECT 1', params = {function() end}})
  return {}
end)
]])
  for _, route in ipairs({'/v/db', '/v/raw'}) do
    expect(s:render(route, 'render-1').result.refresh, route .. ': a clean render').to_be(nil)
    for _, e in ipairs({{code = 'sql_error', message = 'a hub error'}, {code = 'timeout', message = 'lost'}}) do
      s.sql.fail_next['hub.sql.query'] = e
      local r = s:render(route, 'render-2')
      expect(r.error, route .. ' ' .. e.code).to_be(nil)
      expect(r.result.tree.title, route .. ' ' .. e.code .. ': the plugin caught it').to_be('caught')
      expect(r.result.refresh, route .. ' ' .. e.code).to_be('all')
      expect(s:render(route, 'render-3').result.refresh, route .. ' ' .. e.code .. ': the next clean render').to_be(nil)
    end
  end
  local r = s:render('/panels/enc', 'render-4')
  expect(r.error, 'an encode failure: nothing sent').to_be(nil)
  expect(r.result.refresh, 'an encode failure').to_be('all')
end)

test('a read outside SQL in a render replies refresh = "all"; an SQL render stays tracked', function()
  -- The SDK wraps each function that reads outside state when it loads (lazyrex.lua, "External reads
  -- in a render"). srv and alias are made after the SDK loaded, before the render.
  local s = plugin([[
app.db.migrate {'CREATE TABLE t (id INTEGER PRIMARY KEY)'}
local srv = rex.server('lab')
local alias = io.open
local inputs = {
  open = function() local f = io.open('/dev/null'); if f then f:close() end end,
  lines = function() for _ in io.lines('/dev/null') do end end,
  popen = function() pcall(io.popen, 'true') end,
  execute = function() pcall(os.execute, 'true') end,
  call = function() rex.call('x.read', {}) end,
  invoke = function() rex.invoke('x.action') end,
  wait = function() rex.wait('x.event') end,
  servers = function() rex.servers() end,
  host = function() rex.host({}) end,
  exec = function() rex.exec({'/bin/true'}) end,
  exec_start = function() rex.exec.start({'/bin/true'}) end,
  block = function() rex.block.call('creator', 'title', {}) end,
  session = function() rex.session.view({}) end,
  server = function() rex.server('lab') end,
  server_call = function() srv.call('x.read', {}) end,
  server_session = function() srv.session.view({}) end,
  server_exec = function() srv.exec({'/bin/true'}) end,
  server_exec_start = function() srv.exec.start({'/bin/true'}) end,
  alias = function() local f = alias('/dev/null'); if f then f:close() end end,
  app_rex = function() app.rex('x.read') end,
  hub = function() app.hub('hub.thing', {}) end,
  sql = function() app.db.first('SELECT id FROM t'); app.hub('hub.sql.query', {sql = 'SELECT id FROM t', params = {}}) end,
}
app.view('/x/*', {render = function(ctx)
  inputs[ctx.route:sub(4)]()
  return {title = 'X', nodes = {}}
end})
]], {hub = {['hub.thing'] = function() return {} end}})
  local names = {}
  expect(s:render('/x/sql', 'render-0').result.refresh, 'SQL only').to_be(nil)
  expect(#s.rex_calls, 'the SDK transport calls no wrapped function').to_be(1) -- rex.server at load
  for _, n in ipairs({'open', 'lines', 'popen', 'execute', 'call', 'invoke', 'wait', 'servers', 'host', 'exec', 'exec_start',
      'block', 'session', 'server', 'server_call', 'server_session', 'server_exec', 'server_exec_start', 'alias', 'app_rex', 'hub'}) do
    local r = s:render('/x/' .. n, 'render-' .. n)
    expect(r.error, n).to_be(nil)
    expect(r.result.refresh, n).to_be('all')
    expect(s:render('/x/sql', 'render-sql').result.refresh, n .. ': the next SQL render').to_be(nil)
  end
  -- rex.call goes to the sandbox's client methods, which record nothing.
  expect(s.rex_calls, 'each wrapper called the original').to_equal({'server', 'invoke', 'wait', 'servers', 'host', 'exec',
    'exec.start', 'block.call', 'session.view', 'server', 'lab:call', 'lab:session.view', 'lab:exec', 'lab:exec.start'})
end)

test('a wrapper passes values, nil values and errors unchanged; outside a render it marks nothing', function()
  local s = plugin([[
app.action('p.io', {}, function()
  local f, err, code = io.open('/nonexistent/x')
  local ok, raised = pcall(rex.call, 'p.boom', {})
  local e = rex.exec({'/bin/true'})
  local h = rex.exec.start({'/bin/true'})
  local srv, second = rex.server('lab')
  local sok, serr = pcall(rex.server, 'bad')
  return {server = type(srv), second = second, sok = sok, serr = serr, f = f == nil, err = err, code = code, n = select('#', io.open('/nonexistent/x')), ok = ok, raised = tostring(raised), exec = e.name, start = h.name}
end)
]], {rex = {['p.boom'] = function() error('boom-x', 0) end}})
  local want_n = select('#', io.open('/nonexistent/x'))
  local r = s:agent('p.io', {})
  expect(r.error).to_be(nil)
  local _, err, code = io.open('/nonexistent/x')
  expect(r.result).to_equal({f = true, err = err, code = code, n = want_n, ok = false, raised = 'boom-x', exec = 'exec', start = 'exec.start',
    server = 'table', second = 'second', sok = false, serr = 'unknown server bad'})
  expect(r.result.refresh, 'an action reply has no refresh').to_be(nil)
end)

test('if a replacement does not hold, every render replies refresh = "all" (fail closed)', function()
  local s = todo({source = 'local app = require("lazyrex").plugin("p")\napp.panel("P", function() return {} end)', frozen = true})
  expect(s:render('/panels/p', 'render-1').result.refresh).to_be('all')
  expect(find(s.printed, function(l) return l:find('cannot detect reads outside SQL', 1, true) ~= nil end) ~= nil, show(s.printed)).to_be(true)
end)

test('a lost reply is unavailable: client_unavailable, and a table error with timeout, disconnected or client_unavailable', function()
  -- Rex fails a waiting request with client_unavailable when a connection drops (rex-protocol.md,
  -- client.perform; a reply frame over the limit closes the client). The outcome is unknown, as for a
  -- timeout (Automations implementer report, 2026-10-22).
  local sdk = sandbox(sdk_dir, base, {source = 'return 1'}).sdk
  local function code(err) local _, e = pcall(sdk.rex_error, err, 'x failed'); return e.code end
  expect(code('client.perform failed: disconnected while the request waited (status=503 code=client_unavailable)')).to_be('unavailable')
  expect(code('client.perform failed: client_unavailable: the client left (status=503 code=client_unavailable)')).to_be('unavailable')
  for _, c in ipairs({'timeout', 'disconnected', 'client_unavailable'}) do
    expect(code({code = c, message = 'lost'}), c).to_be('unavailable')
  end
  expect(code({code = 'not_found', message = 'gone'}), 'another table code stays').to_be('not_found')
  expect(code('client.perform failed: timeout: statement timeout (status=500 code=internal)'), 'a hub.sql timeout stays timeout (rolled back)').to_be('timeout')
end)

test('a form command that names user_only keeps it; the SDK takes no action from the submit tool', function()
  -- The hub parity check refuses a command with both action and user_only (hub/sql-r2 c840c92,
  -- testmode.go parityProblem; codex-15 review of e258d8d).
  local s = plugin([[
app.action('p.add', {}, function() return {} end)
app.form('/forms/add', {title = 'Add', fields = {}, on_submit = function() end,
  submit = {tool = 'p.add'}, command = {id = 'add', title = 'Add', user_only = 'navigation'}})]])
  local c = s.registered.commands[1]
  expect(c.user_only).to_be('navigation')
  expect(c.action, 'no action next to user_only').to_be(nil)
end)

test('concat gives what table.concat gives, also for 2560 items and more', function()
  -- Rex Lua: table.concat of 2560 items or more fails with "registry overflow" (observed on Rex
  -- 0.0.1792158932-g88fd41, lab, 2026-10-22), and fewer when more of the stack is in use (2559
  -- failed here). Up to 2000 items, the Rex table.concat is the reference. Both are called as a
  -- field, so that an error names the function as "concat".
  local R, C = {concat = table.concat}, {concat = sandbox(sdk_dir, base, {source = 'return 1'}).sdk.concat}
  local function raw(...) return R.concat(...) end
  local function concat(...) return C.concat(...) end
  local function err(fn, ...) local ok, e = pcall(fn, ...); expect(ok, 'an error').to_be(false); return (tostring(e):gsub('^[^\n]-:%d+: ', '', 1)) end
  local function list(n) local t = {}; for k = 1, n do t[k] = tostring(k % 10) end; return t end
  local cases = {{{}}, {{'a'}, ','}, {{'a', 'b', 'c'}, ', ', 2, 3}, {{'a', 'b'}, ',', 3, 2}, {{1, 2.5, 'x'}, '-'},
    {list(1)}, {list(500), ','}, {list(501), ','}, {list(1001), ','}, {list(2000), ','}, {list(2000), ',', 7, 2000}}
  for n, c in ipairs(cases) do expect(concat(unpack(c, 1, 4)), 'case ' .. n).to_be(raw(unpack(c, 1, 4))) end
  local function slow(t, sep, i, j) local s = ''; for k = i, j do s = s .. t[k] .. (k < j and sep or '') end; return s end
  for _, n in ipairs({2560, 10000}) do
    local t = list(n)
    expect(concat(t, ','), n .. ' items').to_be(slow(t, ',', 1, n))
    expect(concat(t), n .. ' items, no separator').to_be(slow(t, '', 1, n))
  end
  local t = list(10000)
  expect(concat(t, '; ', 500, 9000), 'a long range').to_be(slow(t, '; ', 500, 9000))
  t[2500] = {}
  expect(err(concat, t, ','), 'a bad item names its own index').to_be(err(raw, t, ',', 2001, 3000))
  expect(err(concat, {'a', {}}), 'a short list').to_be(err(raw, {'a', {}}))
  expect(err(concat, nil), 'not a table').to_be(err(raw, nil))
  for _, a in ipairs({{false, 2}, {1, false}, {'x', 2}}) do
    expect(err(concat, {'a', 'b'}, ',', a[1], a[2]), 'a bad range argument').to_be(err(raw, {'a', 'b'}, ',', a[1], a[2]))
  end
end)

test('boot.lua sets table.concat to the SDK concat before it runs the main file', function()
  local sdk = sandbox(sdk_dir, base, {source = 'return 1'}).sdk
  local env = {rex = {args = {main = '/plugin/main.lua'}}, package = {path = ''}, debug = debug, assert = assert, type = type,
    table = {concat = table.concat}}
  env.require = function(name) expect(name).to_be('lazyrex'); return sdk end
  env.dofile = function(path) env.ran, env.concat_at_main = path, env.table.concat end
  local chunk = assert(loadfile(sdk_dir .. '/boot.lua'))
  setfenv(chunk, env)
  chunk()
  expect(env.ran).to_be('/plugin/main.lua')
  expect(env.concat_at_main == sdk.concat, 'the main file sees the SDK concat').to_be(true)
end)

test('every read of a render carries its render ID: db.first, the db.each pages and each part of a form', function()
  -- The hub attributes a hub.sql read to a view render by args.render, and re-renders a view only
  -- when a commit changes a table that its last render read (hub lead and claude-22, 2026-10-22).
  local seen = {calls = {}}
  local s = plugin([[
app.panel('Reads', function()
  local items = {}
  local first = app.db.first('SELECT n FROM t')
  app.db.each('SELECT n FROM t', function(row) items[#items + 1] = {id = row.n, label = tostring(row.n)} end)
  return {app.ui.list('reads', items, {empty = tostring(first.n)})}
end, {id = 'reads'})
local function n(part) return part .. ' ' .. app.db.first('SELECT n FROM t WHERE part = ?', part).n end
app.form('/forms/reads', {
  title = function() return n('title') end,
  fields = function() return {{id = 'text', label = n('fields'), kind = 'text', value = ''}} end,
  intro = function() return n('intro') end,
  after = function() return {{id = 'after', text = n('after')}} end,
  instead = function() n('instead'); return nil end,
  on_submit = function() end})]], {hub = paged_hub(seen)})
  local r = s:render('/panels/reads', 'render-7')
  expect(r.error, show(r)).to_be(nil)
  expect(#seen.calls, 'db.first, then the 3 pages of db.each').to_be(4)
  for i, c in ipairs(seen.calls) do expect(c.render, 'panel read ' .. i).to_be('render-7') end
  expect(seen.calls[3].sql:find('OFFSET', 1, true) ~= nil, 'a later page').to_be(true)
  seen.calls = {}
  r = s:render('/forms/reads', 'render-8')
  expect(r.error, show(r)).to_be(nil)
  local parts = {}
  for _, c in ipairs(seen.calls) do
    expect(c.render, 'form read ' .. tostring(c.params[1])).to_be('render-8')
    parts[c.params[1]] = true
  end
  for _, p in ipairs({'title', 'fields', 'intro', 'after', 'instead'}) do expect(parts[p], p).to_be(true) end
end)

test('app.hub adds the render ID to a hub.sql call in a render, and only there', function()
  -- A raw hub.sql read without the ID makes the hub track no reads for that render (hub lead,
  -- 2026-10-22), so the SDK sends the ID on app.hub too. The caller's table does not change.
  local seen = {calls = {}}
  local s = plugin([[
app.panel('Raw', function()
  local a = {sql = 'SELECT n FROM t', params = {}}
  app.hub('hub.sql.query', a)
  app.hub('hub.other', {x = 1})
  return {{id = 'w', text = 'caller render: ' .. tostring(a.render)}}
end, {id = 'raw'})
app.action('p.raw', {}, function() app.hub('hub.sql.query', {sql = 'SELECT n FROM t', params = {}}); return {} end)]],
    {hub = {['hub.sql.query'] = function(a) seen.calls[#seen.calls + 1] = {action = 'query', render = a.render}; return {columns = {'n'}, rows = {}, complete = true} end,
      ['hub.other'] = function(a) seen.calls[#seen.calls + 1] = {action = 'other', render = a.render}; return {} end}})
  local r = s:render('/panels/raw', 'render-5')
  expect(r.error, show(r)).to_be(nil)
  expect(seen.calls[1]).to_equal({action = 'query', render = 'render-5'})
  expect(seen.calls[2], 'not a hub.sql call').to_equal({action = 'other'})
  expect(r.result.tree.nodes[1].text, 'the caller table keeps its fields').to_be('caller render: nil')
  seen.calls = {}
  expect(s:agent('p.raw', {}).error).to_be(nil)
  expect(seen.calls[1], 'outside a render: no ID').to_equal({action = 'query'})
end)

test('a catch inside the SDK does not split the locals of a running plugin handler (Rex Lua pcall defect)', function()
  -- Rex Lua: after pcall catches a Lua error, a closure made earlier and the local of a function that
  -- is still running are separate copies (docs/rex-protocol.md, "Rex Lua runtime"; observed on Rex
  -- 0.0.1792158932-g88fd41). app.refresh catches a failed hub.ui.invalidate while the handler runs.
  local s = plugin([[
app.action('p.split', {}, function()
  local n = 0
  local function inc() n = n + 1 end
  inc()
  app.refresh()
  inc()
  return {n = n}
end)]], {hub = {['hub.ui.invalidate'] = function() return nil, {code = 'unavailable', message = 'no hub'} end}})
  local r = s:agent('p.split', {})
  expect(r.error, show(r)).to_be(nil)
  expect(r.result.n, 'the closure and the handler see one n').to_be(2)
end)

test('a hub call whose arguments cannot be encoded fails with invalid_input, and nothing is sent', function()
  -- hub() checks the encoded size without a protected call: rex.json.encode returns nil and an error
  -- for a Lua function (observed on Rex 0.0.1792158932-g88fd41, lab, 2026-10-22).
  local s = plugin([[
app.action('p.bad', {}, function() app.hub('hub.thing', {f = function() end}); return {} end)]],
    {hub = {['hub.thing'] = function() error('the call must not reach the hub') end}})
  local before = #s.hub_calls
  local r = s:agent('p.bad', {})
  expect(r.error and r.error.code, show(r)).to_be('invalid_input')
  expect(r.error.message:find('cannot be encoded as JSON', 1, true) ~= nil, r.error.message).to_be(true)
  local sent = {}
  for i = before + 1, #s.hub_calls do sent[#sent + 1] = s.hub_calls[i] end
  expect(table.concat(sent, ' '):find('hub.thing', 1, true), 'hub.thing was not sent').to_be(nil)
end)

test('app.refresh sends hub.ui.invalidate to the hub, and a failure does not fail the handler', function()
  local s = plugin([[
app.action('p.refresh', {}, function() app.refresh(); return {done = true} end)]])
  local r = s:agent('p.refresh', {})
  expect(r.result, show(r)).to_equal({done = true})
  expect(s.invalidated, 'one hub.ui.invalidate').to_be(1)
  local f = plugin([[
app.action('p.refresh', {}, function() app.refresh(); return {done = true} end)]],
    {hub = {['hub.ui.invalidate'] = function() return nil, {code = 'unavailable', message = 'no hub'} end}})
  expect(f:agent('p.refresh', {}).result, 'a refused invalidate is ignored').to_equal({done = true})
end)

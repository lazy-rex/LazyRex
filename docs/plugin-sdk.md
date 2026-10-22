# Writing a LazyRex plugin

Status: **v0**. The module is `sdk/lua/lazyrex.lua`; the to-do plugin below is
`sdk/lua/examples/todo`. It runs on the hub's SQL line (hub/sql-r2 c840c92, lab) and ships with
release 4. Start with this page: it has all that a plain plugin needs. The reference (sql.md), the
test guide (docs/testing.md) and the example give more detail.

A plugin is a Lua file that the hub starts. The SDK module `lazyrex` does the registration, the
calls to the hub, the storage, the transactions and the view refresh for you.

## A complete plugin

`lazyrex.json`:

```json
{"name": "todo", "main": "main.lua", "sdk": "0", "test": {"agents": [{"id": "alice", "name": "Alice"}]}}
```

- `"sdk": "0"` makes the hub start the plugin through the SDK launcher (`sdk/lua/boot.lua`), so
  `require('lazyrex')` works.
- `test.agents` lists the agents that the lab tests can open with `agent('alice')`.

`main.lua`:

```lua
local app = require('lazyrex').plugin('todo')

-- The schema. Each string is one version. The SDK runs the new ones once, in order.
-- To change the schema later, add a string at the end; never edit an old one.
app.db.migrate {
  'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)',
}

local function toggle(id) app.db.exec('UPDATE todos SET done = 1 - done WHERE id = ?', id) end

-- Actions: agents and the user can call them. The arguments are checked against the schema.
app.action('todo.add', {text = 'string'}, function(ctx, args)
  local r = app.db.exec('INSERT INTO todos (text) VALUES (?)', args.text)
  return {id = r.id}
end)
app.action('todo.toggle', {id = 'integer'}, function(ctx, args) toggle(args.id); return {} end)

-- A panel: the palette command "Todos" opens it. It shows again by itself after a write.
app.panel('Todos', function(view)
  local items = {}
  for _, t in ipairs(app.db.query('SELECT id, text, done FROM todos ORDER BY id')) do
    items[#items + 1] = {id = t.id, label = t.text, detail = t.done == 1 and 'done' or '',
      tool = 'todo.toggle', on_select = function(ctx) toggle(t.id) end}
  end
  return {app.ui.list('todos', items)}
end)
```

That is the whole plugin. An agent adds an item with `todo.add {text = "buy milk"}`; the user
selects an item in the panel to toggle it. `detail` is the second line of an item. An item `id`
can be a number or a string: the SDK sends it as a string (`tostring`), so a test selects
`item_id = tostring(r.id)`.

- **Migrations.** Call `app.db.migrate` once, at the top level of the main file. Entry N is schema
  version N. The SDK runs the new entries at the start, in one transaction, and then sets
  `PRAGMA user_version`. A failing entry undoes all of them; the plugin prints the error and stops.
  A rollback runs the previous release on the newer schema: the SDK logs one line and runs on. So
  each entry must only expand the schema (a new table, a nullable column or a column with a
  DEFAULT, an index). Make a destructive change in a later release (docs/sql.md section 1).
  A later version adds an entry at the end:

  ```lua
  app.db.migrate {
    'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)',
    'ALTER TABLE todos ADD COLUMN note TEXT', -- version 2
  }
  ```

- **Agent parity.** Each thing that the user can do, an agent can do too. An item with `on_select`
  names the agent action that does the same in `tool` (here `todo.toggle`); both call one function.
  An item that only navigates gives `user_only = 'navigation'` instead.
- **Names.** `app.panel('Todos')` gets the route `/panels/todos` and the palette command `todos`:
  the title in lowercase, with `_` between words (`'My Notes'`: `/panels/my_notes`, `my_notes`).
  `{id = 'x'}` as the third argument sets it. A command id uses only a-z, 0-9 and `_`; an action
  name can have dots (`todo.add`).
- **Schema types.** The short form takes the JSON Schema type names: `'string'`, `'integer'`,
  `'number'`, `'boolean'`, `'object'`, `'array'`. A `?` at the end makes the argument optional
  (`'string?'`).

## The rules

- **`app.db.exec(sql, ...)`** runs one statement. It returns `{changes, id}` (`id`: the new row).
- **`app.db.query(sql, ...)`** returns a list of rows. A row is a table keyed by column name. A NULL
  value is `nil`. Use `?` in the SQL and give the values after it; never build SQL from text.
  A result too big for one reply (about 1 MB) fails with `too_large`; it never returns part of the
  rows.
- **`app.db.each(sql, ..., fn)`** calls `fn(row)` for every row of a result of any size (migrations,
  scans). It reads all pages in one transaction first, then calls `fn`. Each page runs the query again
  (OFFSET paging), so a big result costs pages x rows. Give `ORDER BY` for a defined order.
- **One action, one transaction.** Everything that an action or a panel handler does in the
  database commits when the handler returns, and is undone when it raises an error. A handler sees
  its own writes. Nothing that another handler does can come between its reads and its writes.
- **Errors.** `error('text')` in a handler gives the caller an error with that text. The database
  changes of that handler are undone. A call to another service (for example a message that the
  handler sent) is not undone.
- **Long lists.** Under the SDK, `table.concat` has no length limit (Rex Lua alone fails at about
  2560 items). Do not `unpack` a list of more than about 2000 items, and do not pass that many
  arguments: Rex Lua fails with "registry overflow".
- **Big numbers.** Lua holds integers exactly only up to 2^53. A larger integer from the database
  comes back as a decimal string.
- **Panels refresh by themselves.** You never call a refresh. Keep a panel function fast: it runs
  again after each write.
- **A render only reads.** A panel, view or form render reads the committed state, with no
  transaction. A write (`db.exec`, `db.batch`, `db.transaction`, `app.notice`) in a render fails with `refused`.
  Write in an action or an `on_select` handler.
- **Clicks.** An item's `on_select` runs when the user selects it. The SDK runs the panel function
  again first, so the handler sees the current row. An item that is gone gives a visible error.
- **Empty lists.** An `app.ui.list` with no items renders as a widget with the text "Nothing here
  yet." (`app.ui.list(id, items, {empty = 'No notes'})` sets the text). It has no `items` then:
  in a test, `node(tree, id).text` is the text and `node(tree, id).items` is nil.
- **View size.** A rendered panel, view or form over 48 KiB (encoded, with its status) fails with
  `too_large`: Rex delivers no message over 64 KiB. Show fewer items, or page them.
- **Callers.** `ctx.agent` is the calling agent (`nil` for the user). An action that only the user may
  run: `app.action(name, schema, fn, {user_only = true})`.
- **Retries.** A UI action that the user's renderer sends again runs once. An agent gives a `key` in
  the arguments (`todo.add {text = "buy milk", key = "k1"}`; the SDK adds `key` to every schema).
  A call again with the same `key` and the same arguments gets the first answer and changes nothing.
  The answer is the same table: after `r1 = a:call('todo.add', {text = 'x', key = 'k1'})`, the same
  call gives `r2.id == r1.id`. The same `key` with other arguments fails with `operation_conflict`.

## Test it

`tests/todo.lua`:

```lua
-- A panel opens from its palette command in a pane (it is not a tab).
local function panel()
  local s = session('todo')
  local w = rex.call('session.new_window', {session_id = s, window_label = 'work',
    layout = {block = {flavor = 'com.superlogical.terminal.shell', options = {command = {'/bin/sleep', '600'}, login = false}}}})
  user:command('todos', {session = s, block = w.block_ids[1]})
  return user:view('/panels/todos', 'todo', s)
end

test('an added item shows in the panel, and a select toggles it', function()
  local alice = agent('alice')
  local r = alice:call('todo.add', {text = 'buy milk', key = 'k1'})
  local v = panel()
  local item = node(v:render(), 'todos').items[1]
  expect(item.label).to_be('buy milk')
  local op = v:action('todos', 'select', {item_id = tostring(r.id)})
  expect(op.status).to_be('succeeded')
  expect(node(v:render(), 'todos').items[1].detail).to_be('done')
end)
```

Run `lazyrex-hub test` in the plugin directory (`-file todo.lua` runs one file, `-run <pattern>`
some tests, `-lazyrex <dir>` names the LazyRex checkout when it is not `../LazyRex`). It starts its
own lab; it never touches your terminals.

- **One lab per file.** Each `tests/*.lua` file gets a new lab and hub. The tests in one file share
  it and run in order, so a later test sees the rows of an earlier one.
- **Parity sweep.** Before the files, the run opens every palette command and renders every view.
  Each command names `action` (an agent action) or `user_only`, and each item, widget and form
  action names `tool` (an exported agent action) or `user_only`. A violation fails the run.
- **The count.** Each file reports one more passed check than it has tests: the file checks
  (injections consumed, faults fired, parity of the rendered trees).
- **A failure.** The output shows the tail of `runner.log` and `hub.log`, and the harness deletes
  the lab. To read more, run with `-keep`, and read the logs in the kept lab directory that the
  output names. Delete the directory after. Use `-keep` also when the plugin does not start
  (`plugin todo is not registered`).

The test functions:

| Call | What it does |
|---|---|
| `agent(id)` | An agent from `test.agents`. `a:call(action, args)` calls an action as that agent. |
| `session(name)` | A new lab session; it returns its id. |
| `user:command(id, {session, block})` | Runs a palette command from that pane, as the user does. |
| `user:view(route, plugin, session)` | The open view of that route. |
| `v:render()` | The current tree. `node(tree, id)` finds a node by id. |
| `v:action(node_id, action, input, key)` | A user action. It returns `{status, result or error}` (`status`: `succeeded` or `failed`; `error` is `{code, message}`). |
| `rex.call(method, args)` | A raw Rex call on the lab server (for example `session.new_window`). |
| `expect(x).to_be(y)`, `.to_equal(y)`, `expect_error(fn, pattern)` | Assertions. |

- Select an item: `v:action('todos', 'select', {item_id = '1'})` (the list id and the item id).
- Submit a form (`form` is the form node id, `submit` its action name). A form opens from its
  palette command in a pane, as a panel does:

  ```lua
  user:command('add', {session = s, block = w.block_ids[1]})
  local f = user:view('/forms/add', 'todo', s)
  expect(f:action('form', 'submit', {values = {text = ''}}).error.code).to_be('invalid_input')
  expect(f:action('form', 'submit', {values = {text = 'eggs'}}, 'f1').status).to_be('succeeded')
  ```

The whole lab test of this plugin is `sdk/lua/examples/todo/tests/todo.lua`. All test functions:
docs/testing.md.

## Start a new plugin

Copy `sdk/lua/examples/todo` and change the name in `lazyrex.json`, in `lazyrex.plugin(...)` and in
the action names. (Proposed, not built: `lazyrex-hub plugin new` and `plugin dev`.)

## Forms

A plain form has a title, fields and `on_submit`, and a `command` that opens it. Nothing else is
required.

```lua
app.form('/forms/add', {
  title = 'Add Item',
  fields = {{id = 'text', label = 'Text', kind = 'text', required = true, value = ''}},
  on_submit = function(ctx, values) app.db.exec('INSERT INTO todos (text) VALUES (?)', values.text) end,
  command = {id = 'add', title = 'Add Item'}, -- the palette command that opens it
})
```

- Field kinds: `text`, `multiline`, `boolean`, `select`. `title` and `fields` are values or
  functions of `ctx`. `fields` runs at each render, and again at the submit, in its transaction.
- The SDK checks the values before `on_submit`: a required field must be filled in (a required
  boolean must be true), a select value must be one of its options, and an unknown field is refused.
  An empty string is not filled in. A refused submit fails with
  `{code = 'invalid_input', message = 'Fill in <label>'}`.
- An optional field that the user left empty arrives as `''` or as `nil`. Treat both as empty.
- `on_submit` raises errors as an action does: `app.fail('invalid_input', '...')` gives that code
  and message; `error('text')` gives code `failed`. A helper that the action and `on_submit` share
  can use `app.fail`.
- `on_submit` returns nothing to close the form, or a result: `{form = route}`, `{navigate = ...}`,
  `{status = 'running'}` (never saved: the hub calls again until the end).
- An `app.fail` in `title` or `fields` shows its message in place of the form.
- The form has Cancel and the status "Waiting for input".
- A route can carry values: `app.route('/forms/release', {profile = p})` builds
  `/forms/release?profile=p`, and the form gets them decoded as `ctx.query`. A form route matches
  itself with any query.

Common options:

- `submit = {label = 'Add', tool = 'todo.add'}`: the button label, and the agent action that does
  the same (agent parity). Without `tool`, `submit.user_only` gives the reason (default `navigation`).
- `command = {id, title, description, keywords}`: a palette command that opens the form. For agent
  parity, the SDK gives it `action = submit.tool`; without a tool, `user_only = submit.user_only`
  (default `navigation`). `action` or `user_only` in `command` sets it.
- `intro`: a text above the form (a value or a function of `ctx`).
- `cancel = 'Close'` relabels Cancel; `cancel = false` removes it.

### Advanced

Use these only when a plain form cannot do the job. If one plugin needs more, use a raw `app.view`.

- `id` and `label`: the form node ID (default `form`) and its label.
- `submit.enabled = false` (or a function) refuses a stale submit with `conflict`. `submit.refusal`
  (a text or a function) gives its message.
- More nodes: `intro` (above the form) and `after` (below it) take lists of nodes, or functions of
  `ctx`. A node can be `{id, text}`, `app.ui.widget` or `app.ui.list` with `on_select` (and `tool` or
  `user_only`), as in a panel. A click runs `on_select(ctx)` in its transaction, after the SDK
  evaluates the nodes again.
- Another state of the route: `instead = function(ctx)` answers nil (show the form) or
  `{nodes, status}`. Those nodes replace the form; `intro`, `after` and Cancel stay. A submit then
  fails with `conflict`.
- A value that the render fixed (form v1 has no hidden field): a select field with `check = false`
  takes any text. `on_submit` compares it with the current state.
- `tx = false`: as for actions.
- Do not use `store = false` on a form. The SDK checks the values before the plugin can replay its
  own saved reply, so a retry can fail where the first submit passed. A form that needs its own
  operations table is a raw `app.view` (see below).
- An `app.fail` in `intro`, `after` or `instead` also shows its message in place of the form.

## v0: the advanced surface

For what the simple surface does not cover yet. Each one keeps the caller checks, the structured
errors and the transaction rules above.

| Call | What it does |
|---|---|
| `app.fail(code, message)` | An error with a code (`conflict`, `not_found`, `refused`, ...). `error('text')` gives code `failed`. |
| `app.hub(action, args)`, `app.rex(method, args)` | A raw hub or Rex call. An error raises `{code, message}`; only a lost call is `unavailable`. In a render, a `hub.sql.*` call through `app.hub` carries the render ID, as `app.db` calls do. |
| `app.view(route, handlers, opts)` | A raw view: `render(ctx, args)` answers the whole tree; `['view.action']`, `['view.send']`, ... get `(ctx, args)`. The keymap lists each `view.*` name of the table (for example `view.open`). A route that ends with `*` is a prefix. opts: `tx = false`, `store = false`, as for actions. |
| `app.command(spec)` | An extra palette command (`hub.register` commands). |
| `app.on(event, fn)`, `app.after(s, fn)`, `app.every(s, fn)`, `app.start(fn, opts)` | A Rex event (or the hub calls `hub.changed`, `hub.prompt`, `view.seen`: `fn(args)`; an error goes back to the hub, which calls again), a timer, start code. Each run is one transaction. Start code runs after `hub.register` and the migrations; `{after_register = true}` runs it after the other start code. |
| `app.db.transaction(fn)`, `app.db.batch(statements)` | Several statements all or nothing, for `{tx = false}` code. |
| `app.db.first(sql, ...)` | The first row, or nil. It reads one reply only, so a big result does not fail. |
| `app.notice(agent, pending, opts)`, `app.notice_read(agent)` | Wake an agent at the commit. `{once = true}` (with `pending = true`) nudges again only after the agent read. `app.notice_read` records that the agent read. Both need a hub with notice v2 (release 4): an older hub ignores `once` and refuses a read. |
| `app.refresh()` | Render the open views again, for state outside SQL. |
| `app.int(decimal)`, `app.blob(bytes)` | An exact 64-bit integer or a BLOB parameter. |
| `app.args`, `app.dir` | The plugin arguments; the plugin directory. |
| `app.registration` | The `hub.register` reply (for example `generation`). It is set before the start functions run. |

Action options: `title`, `description`, `user_only`, `tx = false` (each SQL call alone),
`store = false` (no saved reply: the plugin keeps its own operations table), `fingerprint`.
A plugin that keeps its data in hub.store until it moves to SQL passes `store = false` on each
action and `app.view`: with a key, `store = true` reads `hub.sql.replay` first.
The schema is the short form (`{text = 'string', note = 'string?'}`, or a JSON Schema for a property)
or a full JSON Schema. The SDK adds an optional `key`; the handler gets it as `ctx.key`, not in args.

Saved replies: with a key and `store = true` (the default), a retry gets the first terminal reply,
also with `tx = false`. A retry of an agent also finds a reply saved under one of its legacy IDs
(`ctx.aliases`); a new reply is saved under its current ID. A reply with `status = 'running'` is never saved, so a retry reaches the
handler. With `store = false`, every call reaches the handler.

`ctx`: `agent` (nil for the user), `user`, `key`, `session_id`, `block_id`, `requester_pid`,
`aliases`, `identity` (the hub.identify reply), and for views `route`, `input`, `view_id`, `args`,
`query` and `source`. `ctx.source.pwd` is the source pane directory that the hub read once at the
open (the raw `terminal.pwd` value, a `file://` URI); `ctx.source.pwd_error` is the reason when it
got none; neither is set when the view had no source pane. A refresh or a hub restart keeps them.

A list item or a widget with `on_select` gets an action. Give `tool` (the agent action that does
the same; agent parity) or `user_only` (the reason; default `navigation`).

Start: the hub runs `rex do <sdk>/boot.lua main=<main file>`. boot.lua puts the plugin directory
and the SDK directory in `package.path`, so `require('lazyrex')` and sibling files need no setup.

SDK tests: `rex -S unix:///tmp/rexlab-<team>-<you>/rex.sock --autostart=false do <hub>/sdk/lua/tests/run.lua`
(a lab server; no hub). `pattern=<Lua pattern>` runs some tests.

## More

The reference (sql.md) has the options that most plugins never need: long handlers outside a
transaction, notices to agents, paging large results, explicit render dependencies, and the
hub actions under the SDK.

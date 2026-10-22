# Writing a LazyRex plugin

Status: **v0, in progress** (2026-10-22). The module is `sdk/lua/lazyrex.lua`; the to-do plugin below is
`sdk/lua/examples/todo`. Its tests run against a fake hub.sql (`sdk/lua/tests`); the lab run on the real
hub waits for the hub branch `hub/sql`. A plugin author needs only this page. The details are in
"Reference" (sql.md).

A plugin is a Lua file that the hub starts. The SDK module `lazyrex` does the registration, the
calls to the hub, the storage, the transactions and the view refresh for you.

## A complete plugin

`lazyrex.json`:

```json
{"name": "todo", "main": "main.lua"}
```

`main.lua`:

```lua
local app = require('lazyrex').plugin('todo')

-- The schema. Each string is one version. The SDK runs the new ones once, in order.
app.db.migrate {
  'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)',
}

-- An action: agents and the user can call it. The arguments are checked against the schema.
app.action('todo.add', {text = 'string'}, function(ctx, args)
  local r = app.db.exec('INSERT INTO todos (text) VALUES (?)', args.text)
  return {id = r.id}
end)

-- A panel: a tab in the LazyRex window. It shows again by itself after a write to a table that
-- it read.
app.panel('Todos', function(view)
  local items = {}
  for _, t in ipairs(app.db.query('SELECT id, text, done FROM todos ORDER BY id')) do
    items[#items + 1] = {id = t.id, label = t.text, detail = t.done == 1 and 'done' or '',
      on_select = function(ctx) app.db.exec('UPDATE todos SET done = 1 - done WHERE id = ?', t.id) end}
  end
  return {app.ui.list('todos', items)}
end)
```

That is the whole plugin. An agent adds an item with `todo.add {text = "buy milk"}`; the user
selects an item in the panel to toggle it.

## The rules

- **`app.db.exec(sql, ...)`** runs one statement. It returns `{changes, id}` (`id`: the new row).
- **`app.db.query(sql, ...)`** returns a list of rows. A row is a table keyed by column name. A NULL
  value is `nil`. Use `?` in the SQL and give the values after it; never build SQL from text.
- **One action, one transaction.** Everything that an action or a panel handler does in the
  database commits when the handler returns, and is undone when it raises an error. A handler sees
  its own writes. Nothing that another handler does can come between its reads and its writes.
- **Errors.** `error('text')` in a handler gives the caller an error with that text. The database
  changes of that handler are undone. A call to another service (for example a message that the
  handler sent) is not undone.
- **Big numbers.** Lua holds integers exactly only up to 2^53. A larger integer from the database
  comes back as a decimal string.
- **Panels refresh by themselves.** You never call a refresh. Keep a panel function fast: it runs
  again after each write to a table that it read.
- **Clicks.** An item's `on_select` runs when the user selects it. The SDK runs the panel function
  again first, so the handler sees the current row. An item that is gone gives a visible error.
- **Callers.** `ctx.agent` is the calling agent (`nil` for the user). An action that only the user may
  run: `app.action(name, schema, fn, {user_only = true})`.
- **Retries.** A UI action that the user's renderer sends again runs once. An agent that sends a call
  again with the same `key` gets the first answer.

## Test it

`tests/todo.lua`:

```lua
test('an added item shows in the panel', function()
  local alice = agent('alice')
  alice:call('todo.add', {text = 'buy milk'})
  -- A panel opens from its palette command in a pane (it is not a tab).
  local s = session('todo')
  local w = rex.call('session.new_window', {session_id = s, window_label = 'work',
    layout = {block = {flavor = 'com.superlogical.terminal.shell', options = {command = {'/bin/sleep', '600'}, login = false}}}})
  user:command('todos', {session = s, block = w.block_ids[1]})
  local tree = user:view('/panels/todos', 'todo', s):render()
  expect(node(tree, 'todos').items[1].label).to_be('buy milk')
end)
```

The whole lab test of this plugin is `sdk/lua/examples/todo/tests/todo.lua`.

Run `lazyrex-hub test .` in the plugin directory. It starts its own lab; it never touches your
terminals.

## Start a new plugin

`lazyrex-hub plugin new todo` writes `lazyrex.json`, `main.lua`, a passing test and a README.
`lazyrex-hub plugin dev .` runs the plugin in a lab hub and loads it again on each save.

## Forms

```lua
app.form('/forms/add', {
  title = 'Add Item',
  fields = {{id = 'text', label = 'Text', kind = 'text', required = true, value = ''}},
  submit = {label = 'Add', tool = 'todo.add'},
  on_submit = function(ctx, values) app.db.exec('INSERT INTO todos (text) VALUES (?)', values.text) end,
  command = {id = 'add', title = 'Add Item…', description = 'Add an item'},
})
```

- `title`, `fields` and `intro` are values or functions of `ctx`. `intro` is a text, or a list of
  `{id, text}` widgets. `id` and `label` set the form node ID (default `form`) and its label. `fields` runs at each render, and
  again at the submit, in its transaction. Field kinds: `text`, `multiline`, `boolean`, `select`.
- The SDK checks the values before `on_submit`: a required field must be filled in (a required
  boolean must be true), a select value must be one of its options, and an unknown field is refused.
  `app.fail('invalid_input', '...')` in `on_submit` shows another message.
- `on_submit` returns nothing to close the form, or a result: `{form = route}`, `{navigate = ...}`,
  `{status = 'running'}` (never saved: the hub calls again until the end).
- An `app.fail` in `title`, `fields` or `intro` shows its message in place of the form.
- The form has Cancel (`cancel = false` removes it; `cancel = 'Close'` relabels it) and the status
  "Waiting for input". `submit.tool` names the agent action that does the same (agent parity);
  without one, `submit.user_only` gives the reason. `submit.enabled = false` (or a function) refuses a
  stale submit with `conflict`.
- A route can carry values: `app.route('/forms/release', {profile = p})` builds
  `/forms/release?profile=p`, and the form gets them decoded as `ctx.query`. A form route matches
  itself with any query.

## v0: the advanced surface

For what the simple surface does not cover yet. Each one keeps the caller checks, the structured
errors and the transaction rules above.

| Call | What it does |
|---|---|
| `app.fail(code, message)` | An error with a code (`conflict`, `not_found`, `refused`, ...). `error('text')` gives code `failed`. |
| `app.hub(action, args)`, `app.rex(method, args)` | A raw hub or Rex call. An error raises `{code, message}`; only a lost call is `unavailable`. |
| `app.view(route, handlers, opts)` | A raw view: `render(ctx, args)` answers the whole tree; `['view.action']`, `['view.send']`, ... get `(ctx, args)`. The keymap lists each `view.*` name of the table (for example `view.open`). A route that ends with `*` is a prefix. opts: `tx = false`, `store = false`, as for actions. |
| `app.command(spec)` | An extra palette command (`hub.register` commands). |
| `app.on(event, fn)`, `app.after(s, fn)`, `app.every(s, fn)`, `app.start(fn, opts)` | A Rex event (or `hub.changed`, `view.seen`), a timer, start code. Each run is one transaction. Start code runs before the registration; `{after_register = true}` runs it after `hub.register`. |
| `app.db.transaction(fn)`, `app.db.batch(statements)` | Several statements all or nothing, for `{tx = false}` code. |
| `app.db.first(sql, ...)` | The first row, or nil. `app.db.query` also answers `more` (true when a reply did not hold every row). |
| `app.notice(agent, pending)` | Wake an agent at the commit. |
| `app.refresh()` | Render the open views again, for state outside SQL. |
| `app.int(decimal)`, `app.blob(bytes)` | An exact 64-bit integer or a BLOB parameter. |
| `app.args`, `app.dir` | The plugin arguments; the plugin directory. |

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
`aliases`, `identity` (the hub.identify reply), and for views `route`, `input`, `view_id`, `args`.

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

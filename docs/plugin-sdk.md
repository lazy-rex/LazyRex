# Writing a LazyRex plugin

Status: **proposed** (draft 2, 2026-10-21). The SDK does not exist yet. This page is the target:
a plugin author needs only this page. The details are in "Reference" (sql.md).

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
    items[#items + 1] = {id = t.id, label = t.text, detail = t.done == 1 and 'done' or '', action = 'toggle'}
  end
  return {app.ui.list('todos', items)}
end, {
  toggle = function(ctx, item)
    app.db.exec('UPDATE todos SET done = 1 - done WHERE id = ?', item.id)
  end,
})
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
  local tree = user:open('todos'):render()
  expect(H.node(tree, 'todos').items[1].label).to_be('buy milk')
end)
```

Run `lazyrex-hub test .` in the plugin directory. It starts its own lab; it never touches your
terminals.

## Start a new plugin

`lazyrex-hub plugin new todo` writes `lazyrex.json`, `main.lua`, a passing test and a README.
`lazyrex-hub plugin dev .` runs the plugin in a lab hub and loads it again on each save.

## More

The reference (sql.md) has the options that most plugins never need: long handlers outside a
transaction, notices to agents, paging large results, explicit render dependencies, and the
hub actions under the SDK.

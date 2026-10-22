-- A to-do list on the LazyRex Plugin SDK (docs/plugin-sdk.md, "A complete plugin").
local app = require('lazyrex').plugin('todo')

app.db.migrate {
  'CREATE TABLE todos (id INTEGER PRIMARY KEY, text TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0)',
}

app.action('todo.add', {text = {type = 'string', minLength = 1}}, function(ctx, args)
  local r = app.db.exec('INSERT INTO todos (text) VALUES (?)', args.text)
  return {id = r.id}
end, {description = 'Add an item to the to-do list.'})

local function toggle(id)
  local r = app.db.exec('UPDATE todos SET done = 1 - done WHERE id = ?', id)
  if r.changes ~= 1 then app.fail('not_found', 'No item ' .. tostring(id)) end
  return {id = id}
end
app.action('todo.toggle', {id = 'integer'}, function(ctx, args) return toggle(args.id) end,
  {description = 'Mark an item done, or not done again.'})

app.panel('Todos', function(view)
  local items = {}
  for _, t in ipairs(app.db.query('SELECT id, text, done FROM todos ORDER BY id')) do
    items[#items + 1] = {id = t.id, label = t.text, detail = t.done == 1 and 'done' or '', tool = 'todo.toggle',
      on_select = function(ctx) return toggle(t.id) end}
  end
  return {app.ui.list('todos', items, {empty = 'No items. An agent adds one with todo.add.'})}
end)

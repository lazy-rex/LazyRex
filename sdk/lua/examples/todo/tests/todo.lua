-- The to-do plugin on the real hub (lab): the SDK, hub.sql, the panel and agent parity.
local function pane(name)
  local s = session(name)
  local w = rex.call('session.new_window', {session_id = s, window_label = 'work',
    layout = {block = {flavor = 'com.superlogical.terminal.shell', options = {command = {'/bin/sleep', '600'}, login = false}}}})
  return {session = s, block = w.block_ids[1]}
end
local function items(tree) local n = node(tree, 'todos'); return n.items or {} end

test('an agent adds an item, the panel shows it, and a select toggles it', function()
  local alice = agent('alice')
  local r = alice:call('todo.add', {text = 'buy milk', key = 'k1'})
  expect(r.id).to_be(1)
  expect(alice:call('todo.add', {text = 'buy milk', key = 'k1'}).id, 'a retry replays').to_be(1)
  local p = pane('todo-panel')
  user:command('todos', p)
  local v = user:view('/panels/todos', 'todo', p.session)
  local list = items(v:render())
  expect(#list, 'one item: the retry did not add one').to_be(1)
  expect(list[1].label).to_be('buy milk')
  local op = v:action('todos', 'select', {item_id = '1'})
  expect(op.status, show(op)).to_be('succeeded')
  expect(items(v:render())[1].detail).to_be('done')
end)

test('a write by an agent renders the open panel again by itself', function()
  local alice = agent('alice')
  local p = pane('todo-refresh')
  user:command('todos', p)
  local v = user:view('/panels/todos', 'todo', p.session)
  v:render()
  alice:call('todo.add', {text = 'from an agent', key = 'k2'})
  -- v:get() reads the snapshot that the renderer has; nothing here asks for a render.
  wait('the new item in the snapshot', function()
    local tree = (v:get() or {}).tree
    for _, item in ipairs(tree and items(tree) or {}) do if item.label == 'from an agent' then return true end end
  end, 10)
end)

test('a wrong argument and a gone item give visible errors', function()
  local alice = agent('alice')
  local ok, err = pcall(alice.call, alice, 'todo.add', {})
  expect(ok).to_be(false)
  expect(tostring(err)).to_match('text')
  local ok2, err2 = pcall(alice.call, alice, 'todo.toggle', {id = 999, key = 'k3'})
  expect(ok2).to_be(false)
  expect(tostring(err2)).to_match('No item 999')
end)

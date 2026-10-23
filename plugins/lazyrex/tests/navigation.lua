-- Navigation from the Agents… panel and the worker menu. Navigation is user_only ('navigation'): it
-- changes only what the user sees, so it has no agent path. The effects are navigate and form
-- effects that the hub runs and records. A form that a panel opened replaces the panel; the
-- form's end opens the panel again (docs/hub.md, panels).

local codex = agent('codex', {kind = 'codex', title = 'Codex'})

local function find(list, id)
  for _, x in ipairs(list or {}) do if x.id == id then return x end end
end
local function open_view(id)
  for _, w in ipairs(views()) do if w.view_id == id then return w end end
end
local function gone(v) wait(v.route .. ' to close', function() return open_view(v.id) == nil end) end
-- menu opens the worker menu from a new Agents… panel and returns the panel and the menu.
local function menu(spawn_id, key)
  local p = user:panel('agents')
  p:render()
  local op = p:action('workers', 'manage', {item_id = spawn_id}, key)
  expect(op.status).to_be('succeeded')
  expect(op.effects).to_be('done')
  local m = user:view('/forms/worker/' .. spawn_id, nil, p.session)
  m:render()
  return p, m
end
-- The panel is open again in the session, attached: the menu's end reopened it. (The menu replaced
-- the panel, so the check is that the panel comes back, not that both stay mounted.)
local function panel_back(session)
  return user:view('/panels/agents', nil, session)
end

-- First in the file: no thread tab exists yet, so the Message navigation makes the stub declare one.
test('Message while Messaging fails the open: the effect fails, the panel stays; a retry opens it', function()
  local id = fault({point = 'action', action = 'hub.ui.tabs', when = 'before', mode = 'fail', plugin = 'messaging'})
  local p = user:panel('agents')
  local op = p:action('agents', 'message', {item_id = codex.id}, 'nav-message-fail')
  expect(fault_fired(id)).to_be(true)
  expect(op.status).to_be('succeeded')
  expect(op.effects).to_be('failed')
  p:render()
  expect(open_view(p.id), 'the panel stays').to_be_truthy()
  op = p:action('agents', 'message', {item_id = codex.id}, 'nav-message')
  expect(op.status).to_be('succeeded')
  expect(op.result).to_equal({navigate = {plugin = 'messaging', tab = 'thread/' .. codex.id}})
  expect(op.effects).to_be('done')
  expect(tab('thread/' .. codex.id, 'messaging'), 'the messaging plugin opened the thread tab').to_be_truthy()
end)

test('Message with Messaging absent: no Message action; Messaging back: Message again', function()
  H.hub('hub.admin.stop', {plugin = 'messaging'})
  local p = user:panel('agents')
  wait('the panel shows no Message action', function()
    local c = find(H.node(p:render(), 'agents').items, codex.id)
    return c and c.action == nil
  end)
  H.hub('hub.admin.start', {plugin = 'messaging'})
  wait('the panel shows Message again', function()
    local c = find(H.node(p:render(), 'agents').items, codex.id)
    return c and c.action and c.action.name == 'message'
  end)
end)

test('worker menu: Open shows the worker and returns to the panel', function()
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'nav-open', title = 'Opened'})
  local p, m = menu(w.spawn_id, 'nav-open-manage')
  local op = m:action('open', 'open', {}, 'nav-open-run')
  expect(op.status).to_be('succeeded')
  expect(op.result.close_form).to_be(true)
  gone(m)
  panel_back(p.session):render()
end)

test('worker menu: a refused Open keeps the menu with the typed error', function()
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'nav-refused', title = 'Refused'})
  local _, m = menu(w.spawn_id, 'nav-refused-manage')
  local id = fault({point = 'action', action = 'hub.navigate.worker', when = 'before', mode = 'fail', fail_code = 'not_found'})
  local op = m:action('open', 'open', {}, 'nav-refused-run')
  expect(fault_fired(id)).to_be(true)
  expect(op.status).to_be('failed')
  expect(op.error.code, 'the hub code is kept').to_be('not_found')
  m:render()
  expect(open_view(m.id), 'the menu stays').to_be_truthy()
end)

test('worker menu: Escape closes it and returns to the panel', function()
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'nav-escape', title = 'Escaped'})
  local p, m = menu(w.spawn_id, 'nav-escape-manage')
  -- Escape and Cancel end the renderer of a form (view/menu.go cancelForm: tea.Quit). The test
  -- view has no keys: its process ends the same way.
  rex.exec{'kill', tostring(m:status().pid)}
  gone(m)
  panel_back(p.session):render()
end)

test('worker menu: a worker that disappears shows a gone notice and no Stop', function()
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'nav-vanish', title = 'Vanishes'})
  local _, m = menu(w.spawn_id, 'nav-vanish-manage')
  rex.call('block.close', {session_id = w.session_id, block_id = w.block_id})
  wait('the menu shows the worker is gone', function() return H.node(m:render(), 'gone') ~= nil end)
  expect(H.node(m:render(), 'stop'), 'no Stop for a gone worker').to_be(nil)
end)

test('an item that is gone fails with a refresh hint', function()
  local p = user:panel('agents')
  p:render()
  for _, c in ipairs({{'agents', 'message'}, {'workers', 'manage'}, {'workers', 'open'}}) do
    local op = p:action(c[1], c[2], {item_id = 'missing'}, 'nav-missing-' .. c[1] .. c[2])
    expect(op.status).to_be('failed')
    expect(op.error.message).to_match('no longer available')
  end
end)

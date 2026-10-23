-- A plugin for the thread view mode labs (mode_lab_test.go). Its routes (/forms/modelab and the tab
-- /thread/lab) render by the
-- SQL row stage(n): 0 is a read-only thread (a history node and a notice), 1 is an error tree with
-- 12 widget actions, more than a 10-row pane holds, 2 is a thread with 2 widget actions and a
-- composer with a saved draft (so Send is enabled), 3 is a thread whose history holds the rows of
-- msgs(id, author, text) in insert order. An action records its name in clicked(name); a send
-- records 'send'.
-- A command opens only a /forms/ or /panels/ route (hub forms.go). The renderer decides its mode
-- from the tree, never from the route, so stage 0 is a thread frame on this form route.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')

local function hub(action, args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if err or not res then
    error({code = 'hub_failed', message = type(err) == 'table' and (err.message or '') or tostring(err)})
  end
  return res.result
end

local function render(args)
  local row = hub('hub.sql.query', {sql = 'SELECT n FROM stage', params = {}, render = args.render}).rows[1]
  if not row or row[1] == 0 then
    return {tree = {title = 'Mode lab', nodes = {
      {id = 'notice', type = 'widget', text = 'LAB-NOTICE'},
      {id = 'history', type = 'history', page_size = 10, messages = {{id = 'm1', author = 'codex', text = 'LAB-MSG'}}},
    }}}
  end
  if row[1] == 3 then
    local messages = {}
    for _, m in ipairs(hub('hub.sql.query', {sql = 'SELECT id, author, text FROM msgs ORDER BY rowid', params = {}, render = args.render}).rows) do
      messages[#messages + 1] = {id = m[1], author = m[2], text = m[3]}
    end
    return {tree = {title = 'Mode lab', nodes = {
      {id = 'notice', type = 'widget', text = 'LAB-NOTICE'},
      {id = 'history', type = 'history', page_size = 500, messages = messages},
    }}}
  end
  if row[1] == 2 then
    return {tree = {title = 'Mode lab', nodes = {
      {id = 'history', type = 'history', page_size = 10, messages = {{id = 'm1', author = 'codex', text = 'LAB-MSG'}}},
      {id = 'w1', type = 'widget', action = {name = 'do-w1', label = 'Action w1', enabled = true}},
      {id = 'w2', type = 'widget', action = {name = 'do-w2', label = 'Action w2', enabled = true}},
      {id = 'composer', type = 'composer', text = 'DRAFT-TEXT', draft_revision = '1',
        save_action = {name = 'save', enabled = true}, send_action = {name = 'send', enabled = true}},
    }}}
  end
  local nodes = {{id = 'e', type = 'error', text = 'LAB-ERROR the hub lost the thread.'}}
  for k = 1, 12 do
    local id = string.format('w%02d', k)
    nodes[#nodes + 1] = {id = id, type = 'widget', action = {name = 'do-' .. id, label = 'Action ' .. id, enabled = true}}
  end
  return {tree = {title = 'Mode lab', nodes = nodes}}
end

local obj = {type = 'object'}
rex.call('client.keymap', {actions = {
  {name = 'view.render', kind = 'client', title = 'Render', args = obj},
  {name = 'view.action', kind = 'client', title = 'Action', args = obj},
  {name = 'view.send', kind = 'client', title = 'Send', args = obj},
  {name = 'view.draft.save', kind = 'client', title = 'Save draft', args = obj},
}})
rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if ev.requester.client_id ~= hub_id then error({code = 'refused', message = 'Only the hub may call ' .. ev.action}) end
    if ev.action == 'view.render' then return render(ev.args) end
    if ev.action == 'view.action' then
      hub('hub.sql.exec', {sql = 'INSERT INTO clicked (name) VALUES (?)', params = {ev.args.action}})
      return {}
    end
    if ev.action == 'view.send' then
      hub('hub.sql.exec', {sql = 'INSERT INTO clicked (name) VALUES (?)', params = {'send'}})
      return {}
    end
    if ev.action:sub(1, 5) == 'view.' then return {} end
    error({code = 'unknown_action', message = ev.action})
  end)
  if ok then
    rex.call('client.reply', {call_id = ev.call_id, result = result})
  else
    local e = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}
    rex.call('client.reply', {call_id = ev.call_id, error = e})
  end
end)

hub('hub.register', {plugin = 'modelab', instance_id = instance_id,
  commands = {{id = 'mode', title = 'Mode lab', route = '/forms/modelab', user_only = 'a lab view'}}})
-- A tab view: hub.ui.reload reloads tab renderers only (hub ui.go loadViews), so the reload lab uses it.
hub('hub.ui.tabs', {tabs = {{key = 'lab', title = 'Mode lab tab', route = '/thread/lab'}}})
hub('hub.sql.exec', {sql = 'CREATE TABLE IF NOT EXISTS stage (n INTEGER)'})
hub('hub.sql.exec', {sql = 'CREATE TABLE IF NOT EXISTS clicked (name TEXT)'})
hub('hub.sql.exec', {sql = 'CREATE TABLE IF NOT EXISTS msgs (id TEXT, author TEXT, text TEXT)'})
rex.call('client.set_label', {label = 'lazyrex-plugin:modelab'})

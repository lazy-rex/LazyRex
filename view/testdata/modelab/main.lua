-- A plugin for the thread view mode labs (mode_lab_test.go). Its routes (/forms/modelab and the tab
-- /thread/lab) render by the
-- SQL row stage(n): 0 is a read-only thread (a history node and a notice), 1 is an error tree with
-- 12 widget actions, more than a 10-row pane holds, 2 is a thread with 2 widget actions and a
-- composer with a saved draft (so Send is enabled), 3 is a thread whose history holds the rows of
-- msgs(id, author, text) in the order of msg_sort(id, sort), else in insert order, 4 is a thread whose history holds the newest 40 rows of
-- msgs with has_more and total; view.history serves the older rows in pages of 40. An action
-- records its name in clicked(name); a send records 'send', and its text in sent(text).
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

local page_size = 40
local draft_revision = 1 -- the stage 2 composer's draft_revision

-- page returns up to 40 rows of msgs before the row of the ID before (all rows when before is nil),
-- oldest first, and whether older rows exist.
local function page(before, render)
  local where, params = '', {}
  if before then
    where, params = 'WHERE rowid < (SELECT rowid FROM msgs WHERE id = ?)', {before}
  end
  local rows = hub('hub.sql.query', {sql = 'SELECT rowid, id, author, text FROM msgs ' .. where .. ' ORDER BY rowid DESC LIMIT ' .. page_size,
    params = params, render = render}).rows
  local messages = {}
  for i = #rows, 1, -1 do
    messages[#messages + 1] = {id = rows[i][2], author = rows[i][3], text = rows[i][4]}
  end
  local more = false
  if #rows > 0 then
    more = #hub('hub.sql.query', {sql = 'SELECT 1 FROM msgs WHERE rowid < ? LIMIT 1', params = {rows[#rows][1]}, render = render}).rows > 0
  end
  return messages, more
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
    for _, m in ipairs(hub('hub.sql.query', {sql = 'SELECT m.id, m.author, m.text FROM msgs m LEFT JOIN msg_sort s ON s.id = m.id ORDER BY coalesce(s.sort, m.rowid), m.rowid', params = {}, render = args.render}).rows) do
      messages[#messages + 1] = {id = m[1], author = m[2], text = m[3]}
    end
    return {tree = {title = 'Mode lab', nodes = {
      {id = 'notice', type = 'widget', text = 'LAB-NOTICE'},
      {id = 'history', type = 'history', page_size = 500, messages = messages},
    }}}
  end
  if row[1] == 4 then
    local messages, more = page(nil, args.render)
    local total = hub('hub.sql.query', {sql = 'SELECT count(*) FROM msgs', params = {}, render = args.render}).rows[1][1]
    return {tree = {title = 'Mode lab', nodes = {
      {id = 'notice', type = 'widget', text = 'LAB-NOTICE'},
      {id = 'history', type = 'history', page_size = page_size, messages = messages, has_more = more, total = total},
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
  {name = 'view.history', kind = 'client', title = 'History', args = obj},
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
      hub('hub.sql.exec', {sql = 'INSERT INTO sent (text) VALUES (?)', params = {ev.args.text or ''}})
      draft_revision = draft_revision + 1
      return {draft_revision = tostring(draft_revision), message_id = 'sent-' .. draft_revision}
    end
    -- A draft save keeps nothing; its reply carries the saved text and a new revision, as the
    -- renderer requires (model.go: a reply with other text is a conflict).
    if ev.action == 'view.draft.save' then
      draft_revision = draft_revision + 1
      return {text = ev.args.text, draft_revision = tostring(draft_revision)}
    end
    if ev.action == 'view.history' then
      local messages, more = page(ev.args.before)
      return {messages = messages, has_more = more}
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
hub('hub.sql.exec', {sql = 'CREATE TABLE IF NOT EXISTS sent (text TEXT)'})
hub('hub.sql.exec', {sql = 'CREATE TABLE IF NOT EXISTS msgs (id TEXT, author TEXT, text TEXT)'})
-- An explicit thread order for stage 3 (the late lab): a row here sorts its message by sort; a
-- message without one sorts by its insert order (rowid).
hub('hub.sql.exec', {sql = 'CREATE TABLE IF NOT EXISTS msg_sort (id TEXT, sort REAL)'})
rex.call('client.set_label', {label = 'lazyrex-plugin:modelab'})

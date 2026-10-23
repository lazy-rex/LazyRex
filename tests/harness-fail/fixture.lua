-- The plugin of the harness failure fixtures. It has only what their tests use; the harness
-- self-test plugin is tests/harness/fixture.lua. A plugin loads only files inside its own
-- directory (docs/plugin-sources.md, section 2), so this is its own small plugin, not a load of that
-- file.
--   fix.put {key, value}      writes a row of its SQL table kv (parity*.lua: the row mode)
--   fix.hub {action, args}    calls a hub action as the plugin (unreleased.lua)
--   /home                     mode = 'noparity' adds an action with neither tool nor user_only
-- The name harnessfix is the plugin of the injections (unconsumed*.lua).
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')

local function err_code(err)
  local text = type(err) == 'table' and (err.message or '') or tostring(err)
  return type(err) == 'table' and err.code or text:match('code=([%w_%-%.]+)%)%s*$'), text
end

local function hub(action, args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if err or not res then
    local _, text = err_code(err)
    error({code = 'hub_failed', message = text})
  end
  return res.result
end

-- get reads the kv row key. A render passes its render ID, so that the view tracks the table kv.
local function get(key, render)
  local row = hub('hub.sql.query', {sql = 'SELECT value FROM kv WHERE key = ?', params = {key}, render = render}).rows[1]
  return row and rex.json.decode(row[1])
end

local obj = {type = 'object'}
rex.call('client.keymap', {actions = {
  {name = 'hub.forward', kind = 'client', title = 'Forwarded agent call', args = obj},
  {name = 'view.render', kind = 'client', title = 'Render', args = obj},
  {name = 'view.action', kind = 'client', title = 'Action', args = obj},
  {name = 'view.open', kind = 'client', title = 'Open a lazy tab', args = obj},
  {name = 'view.seen', kind = 'client', title = 'The user looks at a view', args = obj},
  {name = 'fix.put', kind = 'client', title = 'Put a kv row', description = 'Write {key, value}.',
    args = {type = 'object', properties = {key = {type = 'string'}, value = {}}, required = {'key', 'value'}}},
  {name = 'fix.hub', kind = 'client', title = 'Call a hub action as the plugin', description = 'Returns {result} or {error = {code, message}}.',
    args = {type = 'object', properties = {action = {type = 'string'}, args = {type = 'object'}}, required = {'action'}}},
}})

local function domain(action, args)
  if action == 'fix.put' then
    hub('hub.sql.exec', {sql = 'INSERT INTO kv VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value',
      params = {args.key, rex.json.encode(args.value)}})
    return {ok = true}
  elseif action == 'fix.hub' then
    -- The hub's answer as data, so a test can check the exact error code.
    local res, err = rex.call('client.perform', {client_id = hub_id, action = args.action, args = args.args or {}, timeout_ms = 10000})
    if err then
      local code, text = err_code(err)
      return {error = {code = code, message = text}}
    end
    return {result = res and res.result}
  end
  error({code = 'not_supported', message = 'unknown action ' .. tostring(action)})
end

local function home_tree(render)
  local mode = get('mode', render)
  local nodes = {{id = 'status', type = 'widget', text = 'mode ' .. tostring(mode or 'normal'),
    action = {name = 'refresh', label = 'Refresh', enabled = true, user_only = 'redraws the view'}}}
  if mode == 'noparity' then
    nodes[#nodes + 1] = {id = 'bare', type = 'widget', text = 'no parity', action = {name = 'poke', label = 'Poke', enabled = true}}
  end
  return {title = 'Fixture', nodes = nodes}
end

local function view(action, args)
  if action == 'view.render' then
    if args.route == '/home' then return {tree = home_tree(args.render)} end
    error({code = 'not_found', message = 'unknown route ' .. tostring(args.route)})
  end
  if action == 'view.action' then error({code = 'invalid_input', message = 'unknown node ' .. tostring(args.node_id)}) end
  return {}
end

rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if ev.action == 'hub.forward' or ev.action:sub(1, 5) == 'view.' then
      if ev.requester.client_id ~= hub_id then error({code = 'refused', message = 'Only the hub may call ' .. ev.action}) end
    end
    if ev.action == 'hub.forward' then return domain(ev.args.action, ev.args.args or {}) end
    if ev.action:sub(1, 5) == 'view.' then return view(ev.action, ev.args) end
    return domain(ev.action, ev.args or {})
  end)
  if ok then
    rex.call('client.reply', {call_id = ev.call_id, result = result})
  else
    local e = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}
    rex.call('client.reply', {call_id = ev.call_id, error = e})
  end
end)

hub('hub.register', {plugin = 'harnessfix', instance_id = instance_id, exports = {'fix.put', 'fix.hub'}})
-- After the registration: hub.sql needs a registered plugin. Before the tabs: /home reads kv.
hub('hub.sql.exec', {sql = 'CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT NOT NULL)'})
hub('hub.ui.tabs', {tabs = {{key = 'home', title = 'Fixture home', route = '/home'}}})
rex.call('client.set_label', {label = 'lazyrex-plugin:harnessfix'})

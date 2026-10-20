-- A plugin that tests hub.watch (docs/hub.md, "File watches") through the real hub. The runner
-- calls wf.hub to make a hub.watch.* call as this plugin, and wf.nudges to read the nudges that
-- the hub sent (hub.changed with what = "watch"), counted by watch key.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')

-- State lives in table fields: a closure write to a captured local can be lost after a caught
-- error (rex-protocol.md, "Rex Lua runtime").
local S = {nudges = {}}

local function hub(action, args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if err or not res then error({code = 'hub_failed', message = tostring(err)}) end
  return res.result
end

local obj = {type = 'object'}
rex.call('client.keymap', {actions = {
  {name = 'view.render', kind = 'client', title = 'Render', args = obj},
  {name = 'hub.changed', kind = 'client', title = 'Hub change notice', args = obj},
  {name = 'wf.hub', kind = 'client', title = 'Call a hub action as the plugin; returns {result} or {error = {code, message}}', args = obj},
  {name = 'wf.nudges', kind = 'client', title = 'The watch nudges, counted by key', args = obj},
}})

local function call_hub(args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = args.action, args = args.args or {}, timeout_ms = 10000})
  if err then
    local text = type(err) == 'table' and (err.message or '') or tostring(err)
    local code = type(err) == 'table' and err.code or text:match('code=([%w_%-%.]+)%)%s*$')
    return {error = {code = code, message = text}}
  end
  return {result = res and res.result}
end

rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    local args = ev.args or {}
    if ev.action == 'view.render' then
      return {tree = {title = 'Watch fixture', nodes = {{id = 'text', type = 'widget', text = 'Watch fixture'}}}}
    elseif ev.action == 'hub.changed' then
      if args.what == 'watch' then S.nudges[args.key] = (S.nudges[args.key] or 0) + 1 end
      return {}
    elseif ev.action == 'wf.hub' then
      return call_hub(args)
    elseif ev.action == 'wf.nudges' then
      return {nudges = S.nudges}
    end
    error({code = 'invalid_input', message = 'unknown action ' .. tostring(ev.action)})
  end)
  if ok then
    rex.call('client.reply', {call_id = ev.call_id, result = result})
  else
    local e = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}
    rex.call('client.reply', {call_id = ev.call_id, error = e})
  end
end)

hub('hub.register', {plugin = 'watchfix', instance_id = instance_id, exports = {}})
hub('hub.ui.tabs', {tabs = {{key = 'home', title = 'Watch fixture', route = '/home'}}})
rex.call('client.set_label', {label = 'lazyrex-plugin:watchfix'})

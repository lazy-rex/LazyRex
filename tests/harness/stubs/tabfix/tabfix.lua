-- A second plugin with one tab, home at /home: the target of the fixture's cross-plugin navigate
-- effects (effects.lua). The home plugin has no tab (docs/hub.md, "No LazyRex tab").
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')

local function hub(action, args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if err or not res then error({code = 'unavailable', message = tostring(err)}) end
  return res.result
end

local obj = {type = 'object'}
rex.call('client.keymap', {actions = {
  {name = 'view.render', kind = 'client', title = 'Render', args = obj},
  {name = 'view.action', kind = 'client', title = 'Action', args = obj},
}})
rex.on('client_perform_requested', function(ctx, ev)
  if ev.requester.client_id ~= hub_id then
    rex.call('client.reply', {call_id = ev.call_id, error = {code = 'refused', message = 'Only the hub may call ' .. ev.action}})
  elseif ev.action == 'view.render' then
    rex.call('client.reply', {call_id = ev.call_id, result = {tree = {title = 'Tab fixture', nodes = {{id = 'route', type = 'widget', text = tostring(ev.args.route)}}}}})
  else
    rex.call('client.reply', {call_id = ev.call_id, error = {code = 'invalid_input', message = 'the stub has no actions'}})
  end
end)
hub('hub.register', {plugin = 'tabfix', instance_id = instance_id, exports = {}})
hub('hub.ui.tabs', {tabs = {{key = 'home', title = 'Tab fixture', route = '/home'}}})
rex.call('client.set_label', {label = 'lazyrex-plugin:tabfix'})

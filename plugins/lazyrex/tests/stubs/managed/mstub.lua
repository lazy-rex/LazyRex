-- A plugin that the Plugins… tests add as a managed entry (tests/plugins.lua): it prints its arg k
-- (the Log panel shows the pane) and has a landing panel (the menu's Open).
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
print('mstub runs with k=' .. tostring(rex.args.k))
local res, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.register', args = {plugin = 'mstub',
  instance_id = instance_id, exports = {}, commands = {{id = 'main', title = 'Mstub…', route = '/panels/main', surface = 'panel', landing = true, user_only = 'navigation'}}}, timeout_ms = 10000})
if err or not res then error('register: ' .. tostring(err)) end
rex.call('client.keymap', {actions = {{name = 'view.render', kind = 'client', title = 'view.render', args = {type = 'object', additionalProperties = true}},
  {name = 'view.seen', kind = 'client', title = 'view.seen', args = {type = 'object', additionalProperties = true}}}})
rex.call('client.set_label', {label = 'lazyrex-plugin:mstub'})
rex.on('client_perform_requested', function(ctx, ev)
  if ev.action == 'view.render' then
    rex.call('client.reply', {call_id = ev.call_id, result = {tree = {title = 'Mstub', nodes = {{id = 'hello', type = 'widget', text = 'mstub panel'}}}}})
  else
    rex.call('client.reply', {call_id = ev.call_id, result = {}})
  end
end)

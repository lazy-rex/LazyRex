-- A plugin that depends on mstub (tests/plugins.lua): while mstub is stopped, it waits.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local res, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.register', args = {plugin = 'mdep',
  instance_id = instance_id, exports = {}, commands = {}}, timeout_ms = 10000})
if err or not res then error('register: ' .. tostring(err)) end
rex.call('client.set_label', {label = 'lazyrex-plugin:mdep'})
rex.on('client_perform_requested', function(ctx, ev) rex.call('client.reply', {call_id = ev.call_id, result = {}}) end)

-- A stand-in for a plugin that offers a launch, as Profiles does: a palette command whose action
-- is hub.spawn (Profiles' Start Agent…). The home plugin registers its own "Launch Profile…" only
-- while no other registered plugin has such a command; the name "launcher" proves that the
-- capability decides, not the name. A test stops and starts the stub with hub.admin.stop and
-- hub.admin.start. The plugin arg commands = none registers it without the command.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local commands = {{id = 'start_agent', title = 'Start Agent…', route = '/forms/start-agent', action = 'hub.spawn'}}
if rex.args.commands == 'none' then commands = {} end
local res, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.register',
  args = {plugin = 'launcher', instance_id = instance_id, exports = {}, commands = commands}, timeout_ms = 10000})
if err or not res then error('register: ' .. tostring(err)) end
rex.call('client.set_label', {label = 'lazyrex-plugin:launcher'})
-- The plugin stays running: it answers every call with a refusal.
rex.on('client_perform_requested', function(ctx, ev)
  rex.call('client.reply', {call_id = ev.call_id, error = {code = 'invalid_input', message = 'the stub has no actions'}})
end)

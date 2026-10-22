-- A stand-in for the Profiles plugin in the home tests. The home plugin registers its own
-- "Launch Profile…" palette entry only while no plugin named profiles is registered. The stub only
-- registers; a test stops and starts it with hub.admin.stop and hub.admin.start.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local res, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.register',
  args = {plugin = 'profiles', instance_id = instance_id, exports = {}}, timeout_ms = 10000})
if err or not res then error('register: ' .. tostring(err)) end
rex.call('client.set_label', {label = 'lazyrex-plugin:profiles'})
-- The plugin stays running: it answers every call with a refusal.
rex.on('client_perform_requested', function(ctx, ev)
  rex.call('client.reply', {call_id = ev.call_id, error = {code = 'invalid_input', message = 'the stub has no actions'}})
end)

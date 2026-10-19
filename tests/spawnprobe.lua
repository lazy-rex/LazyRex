-- Lab plugin for tests/spawn-env-lab.mjs: probe.spawn and probe.stop call hub.spawn and hub.stop
-- for "user". Lab only.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local function hub(action, args)
  local response, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if type(response) ~= 'table' or type(response.result) ~= 'table' then
    error(type(err) == 'table' and err or {code = 'unavailable', message = tostring(err)})
  end
  return response.result
end
rex.call('client.keymap', {actions = {{name = 'probe.spawn', kind = 'client', title = 'probe.spawn',
  args = {type = 'object', properties = {profile = {type = 'string'}, key = {type = 'string'}, cwd = {type = 'string'}}, required = {'profile', 'key'}}},
  {name = 'probe.stop', kind = 'client', title = 'probe.stop',
    args = {type = 'object', properties = {spawn_id = {type = 'string'}}, required = {'spawn_id'}}}}})
hub('hub.register', {plugin = 'spawnprobe', instance_id = instance_id, exports = {}})
rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    local a = ev.args or {}
    if ev.action == 'probe.stop' then return hub('hub.stop', {['for'] = 'user', spawn_id = a.spawn_id}) end
    return hub('hub.spawn', {['for'] = 'user', profile = a.profile, key = a.key, cwd = a.cwd})
  end)
  if ok then rex.call('client.reply', {call_id = ev.call_id, result = result})
  else rex.call('client.reply', {call_id = ev.call_id, error = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}}) end
end)
rex.call('client.set_label', {label = 'lazyrex-plugin:spawnprobe'})

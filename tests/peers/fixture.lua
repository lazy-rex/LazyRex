-- A plugin that tests the receive side of peer hubs (docs/hub.md, "Peer hubs") through the real hub.
-- peer.receive records each envelope; it refuses the key "refuse" with a result and raises for the
-- key "raise". pf.got returns the records; pf.send calls hub.peer.send as this plugin.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')

-- State lives in table fields: a closure write to a captured local can be lost after a caught
-- error (rex-protocol.md, "Rex Lua runtime").
local S = {got = {}}

local function hub(action, args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if err or not res then error({code = 'hub_failed', message = tostring(err)}) end
  return res.result
end

local obj = {type = 'object'}
rex.call('client.keymap', {actions = {
  {name = 'view.render', kind = 'client', title = 'Render', args = obj},
  {name = 'hub.changed', kind = 'client', title = 'Hub change notice', args = obj},
  {name = 'peer.receive', kind = 'client', title = 'An envelope from a peer hub', args = obj},
  {name = 'pf.got', kind = 'client', title = 'The received envelopes', args = obj},
  {name = 'pf.send', kind = 'client', title = 'hub.peer.send as the plugin', args = obj},
}})

rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    local args = ev.args or {}
    if ev.action == 'view.render' then
      return {tree = {title = 'Peer fixture', nodes = {{id = 'text', type = 'widget', text = 'Peer fixture'}}}}
    elseif ev.action == 'hub.changed' then
      return {}
    elseif ev.action == 'peer.receive' then
      if args.key == 'raise' then error({code = 'failed', message = 'the fixture raised'}) end
      S.got[#S.got + 1] = {from_hub = args.from_hub, key = args.key, body = args.body}
      if args.key == 'refuse' then return {refused = 'unknown_recipient', message = 'no such agent'} end
      return {stored = #S.got}
    elseif ev.action == 'pf.got' then
      return {got = S.got}
    elseif ev.action == 'pf.send' then
      return hub('hub.peer.send', args)
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

hub('hub.register', {plugin = 'peerfix', instance_id = instance_id, exports = {}})
hub('hub.ui.tabs', {tabs = {{key = 'home', title = 'Peer fixture', route = '/home'}}})
rex.call('client.set_label', {label = 'lazyrex-plugin:peerfix'})

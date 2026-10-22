-- A stand-in for the messaging plugin in the home tests. The home plugin shows an agent's Message
-- action only when a plugin named messaging is loaded, and it opens that plugin's landing panel.
-- The stub has a landing panel (/panels/threads) and declares thread/<id> tabs on view.open.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')

local function hub(action, args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if err or not res then error({code = 'unavailable', message = tostring(err)}) end
  return res.result
end

local tabs = {}
local obj = {type = 'object'}
rex.call('client.keymap', {actions = {
  {name = 'view.render', kind = 'client', title = 'Render', args = obj},
  {name = 'view.action', kind = 'client', title = 'Action', args = obj},
  {name = 'view.open', kind = 'client', title = 'Open a lazy tab', args = obj},
}})

rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if ev.requester.client_id ~= hub_id then error({code = 'refused', message = 'Only the hub may call ' .. ev.action}) end
    if ev.action == 'view.open' then
      local id = ev.args.route:match('^/thread/(.+)$')
      if id then
        -- A declaration that failed is sent again in full: the list never holds a key twice.
        local list = {}
        for _, t in ipairs(tabs) do if t.key ~= 'thread/' .. id then list[#list + 1] = t end end
        list[#list + 1] = {key = 'thread/' .. id, title = 'Thread ' .. id, route = ev.args.route}
        hub('hub.ui.tabs', {tabs = list})
        tabs = list
      end
      return {}
    end
    if ev.action == 'view.render' then
      return {tree = {title = 'Stub ' .. tostring(ev.args.route), nodes = {{id = 'route', type = 'widget', text = tostring(ev.args.route)}}}}
    end
    error({code = 'invalid_input', message = 'the stub has no actions'})
  end)
  if ok then
    rex.call('client.reply', {call_id = ev.call_id, result = result})
  else
    local e = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}
    rex.call('client.reply', {call_id = ev.call_id, error = e})
  end
end)

hub('hub.register', {plugin = 'messaging', instance_id = instance_id, exports = {},
  commands = {{id = 'threads', title = 'Threads', route = '/panels/threads', surface = 'panel', landing = true, user_only = 'navigation'}}})
rex.call('client.set_label', {label = 'lazyrex-plugin:messaging'})

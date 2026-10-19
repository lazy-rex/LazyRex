-- Lab test plugin for the UI reconciler. It declares tabs, and the test changes them with
-- `rex -C lazyrex-plugin:uitest do uitest.set_tabs --args '{"tabs":[...]}'`.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')

local function hub(action, args)
  local response = rex.call('client.perform', {client_id = hub_id, action = action, args = args, timeout_ms = 10000})
  return response.result
end

rex.call('client.keymap', {actions = {
  {name = 'uitest.set_tabs', kind = 'client', title = 'Declare tabs',
    args = {type = 'object', properties = {tabs = {type = 'array'}}, required = {'tabs'}}}
}})
hub('hub.register', {plugin = 'uitest', instance_id = instance_id, exports = {}})
hub('hub.ui.tabs', {tabs = {
  {key = 'home', title = 'Test home', route = '/'},
  {key = 't1', title = 'Thread 1', route = '/thread/1'},
  {key = 't2', title = 'Thread 2', route = '/thread/2'},
}})
rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if ev.action == 'uitest.set_tabs' then return hub('hub.ui.tabs', {tabs = ev.args.tabs}) end
    error({code = 'not_supported', message = 'unknown action'})
  end)
  if ok then
    rex.call('client.reply', {call_id = ev.call_id, result = result})
  else
    rex.call('client.reply', {call_id = ev.call_id, error = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}})
  end
end)
rex.call('client.set_label', {label = 'lazyrex-plugin:uitest'})

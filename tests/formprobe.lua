-- Lab plugin for tests/palette-form-lab.mjs. LAB ONLY. It declares one palette command that opens a
-- form, and shows the stored form context so the test can read it from the screen.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local function fail(code, message) error({code = code, message = message}) end
local function hub(action, args)
  local response, call_error = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if type(response) ~= 'table' then fail('unavailable', tostring(type(call_error) == 'table' and call_error.message or call_error)) end
  return response.result
end
local function action(name, label) return {name = name, label = label, enabled = true} end
local function ctx_text(c)
  if type(c) ~= 'table' then return 'context: none' end
  return 'context: session=' .. tostring(c.session_id) .. ' window=' .. tostring(c.window_id)
    .. ' source=' .. tostring(c.source_view_id) .. ' route=' .. tostring(c.source_route)
end
local function render(args)
  if args.route == '/' then
    return {title = 'Form probe', nodes = {{id = 'open', type = 'widget', text = 'probe-home', action = action('open', 'Open form')}}}
  elseif args.route == '/forms/probe' then
    return {title = 'Probe form', nodes = {{id = 'chain', type = 'widget', text = 'probe-form-1 ' .. ctx_text(args.context), action = action('chain', 'Next form')},
      {id = 'bad', type = 'widget', text = 'bad form route', action = action('bad', 'Bad form')},
      {id = 'badnav', type = 'widget', text = 'bad navigation', action = action('badnav', 'Bad navigation')}}}
  elseif args.route == '/forms/second' then
    return {title = 'Second form', nodes = {{id = 'done', type = 'widget', text = 'probe-form-2 ' .. ctx_text(args.context), action = action('done', 'Done')}}}
  end
  fail('not_found', 'Unknown route')
end
local function perform(args)
  if args.route == '/' and args.action == 'open' then return {form = '/forms/probe'} end
  if args.route == '/forms/probe' and args.action == 'chain' then return {form = '/forms/second'} end
  if args.route == '/forms/probe' and args.action == 'bad' then return {form = '/not-a-form'} end
  if args.route == '/forms/probe' and args.action == 'badnav' then return {navigate = 'no-such-tab'} end
  if args.route == '/forms/second' and args.action == 'done' then return {close_form = true} end
  fail('invalid_input', 'Unknown action')
end
-- Lab only: re-declare the tabs with a 'homed' tab in a home session (hometabs-lab.mjs).
local function declare(args)
  local tabs = {{key = 'home', title = 'Form probe', route = '/'}}
  if type(args.session_id) == 'string' then
    tabs[#tabs + 1] = {key = 'homed', title = 'Homed', route = '/', session_id = args.session_id ~= '' and args.session_id or nil, revision = args.revision}
  end
  hub('hub.ui.tabs', {tabs = tabs})
  return {declared = #tabs}
end
local handlers = {
  ['view.render'] = function(args) return {tree = render(args)} end,
  ['view.action'] = perform,
  ['view.seen'] = function() return {} end,
  ['hub.changed'] = function() return {} end,
}
local actions = {}
for name in pairs(handlers) do actions[#actions + 1] = {name = name, kind = 'client', title = name,
  args = {type = 'object', additionalProperties = true}} end
actions[#actions + 1] = {name = 'formprobe.declare', kind = 'client', title = 'formprobe.declare', args = {type = 'object', additionalProperties = true}}
rex.call('client.keymap', {actions = actions})
hub('hub.register', {plugin = 'formprobe', instance_id = instance_id, exports = {},
  commands = {{id = 'probe', title = 'Probe Form…', description = 'Lab probe: open a form', keywords = {'probe', 'lab "quoted"'}, route = '/forms/probe'}}})
hub('hub.ui.tabs', {tabs = {{key = 'home', title = 'Form probe', route = '/'}}})
rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if ev.action == 'formprobe.declare' then return declare(ev.args or {}) end -- lab only: any caller
    if not ev.requester or ev.requester.client_id ~= hub_id then fail('refused', 'Only the hub calls probe actions') end
    local handler = handlers[ev.action]
    if not handler then fail('not_supported', 'Unknown action') end
    return handler(ev.args or {})
  end)
  if ok then rex.call('client.reply', {call_id = ev.call_id, result = result})
  else rex.call('client.reply', {call_id = ev.call_id, error = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}}) end
end)
rex.call('client.set_label', {label = 'lazyrex-plugin:formprobe'})

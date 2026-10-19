-- Lab plugin for tests/panels-lab.mjs case F. LAB ONLY. A landing panel opens a form with two form
-- nodes. Submit of the first node ends the form; the second node's note stays unsaved, so the
-- return to the panel asks the user (Codex's lab shape for Keep editing -> new submit -> return).
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local function fail(code, message) error({code = code, message = message}) end
local function hub(action, args)
  local response, call_error = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if type(response) ~= 'table' then fail('unavailable', tostring(type(call_error) == 'table' and call_error.message or call_error)) end
  return response.result
end
local function action(name, label) return {name = name, label = label, enabled = true} end
local function render(args)
  if args.route == '/panels/kp' then
    return {title = 'Keep probe', nodes = {{id = 'edit', type = 'widget', text = 'kp-panel', action = action('edit', 'Open the form')}}}
  elseif args.route == '/forms/two' then
    return {title = 'Two nodes', nodes = {
      {id = 'one', type = 'form', fields = {{id = 'a', label = 'Field A', kind = 'text', value = ''}}, submit_action = action('submit', 'Submit one')},
      {id = 'two', type = 'form', fields = {{id = 'note', label = 'Note', kind = 'text', value = ''}}, submit_action = action('save', 'Save note')}}}
  end
  fail('not_found', 'Unknown route')
end
local function perform(args)
  if args.route == '/panels/kp' and args.action == 'edit' then return {form = '/forms/two'} end
  if args.route == '/forms/two' and args.action == 'submit' then return {close_form = true} end
  if args.route == '/forms/two' and args.action == 'save' then return {} end
  fail('invalid_input', 'Unknown action')
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
rex.call('client.keymap', {actions = actions})
hub('hub.register', {plugin = 'keepprobe', instance_id = instance_id, exports = {},
  commands = {{id = 'kp', title = 'Keep Probe…', description = 'Lab probe panel', keywords = {'probe'}, surface = 'panel', landing = true, route = '/panels/kp'}}})
hub('hub.ui.tabs', {tabs = {}})
rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if not ev.requester or ev.requester.client_id ~= hub_id then fail('refused', 'Only the hub calls probe actions') end
    local handler = handlers[ev.action]
    if not handler then fail('not_supported', 'Unknown action') end
    return handler(ev.args or {})
  end)
  if ok then rex.call('client.reply', {call_id = ev.call_id, result = result})
  else rex.call('client.reply', {call_id = ev.call_id, error = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}}) end
end)
rex.call('client.set_label', {label = 'lazyrex-plugin:keepprobe'})

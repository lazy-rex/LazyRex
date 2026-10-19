-- LazyRex home. The hub owns profiles, workers, and navigation.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local navigation_notice
local function fail(code, message) error({code = code, message = message}) end
local function hub(action, args)
  local response, call_error = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if type(response) ~= 'table' then
    if type(call_error) == 'table' then fail(call_error.code or 'unavailable', call_error.message or 'Hub request failed') end
    fail('unavailable', tostring(call_error or 'Hub request failed'))
  end
  if type(response.result) ~= 'table' then fail('internal', 'Invalid hub result') end
  return response.result
end
-- Every user action names its agent tool, or why it is user-only (docs/testing.md, "Agent parity").
-- Navigation changes only what the user sees; agents start and stop workers with the hub tools.
local function action(name, label, parity)
  local a = {name = name, label = label, enabled = true}
  for k, v in pairs(parity) do a[k] = v end
  return a
end
local navigation = {user_only = 'navigation'}
local function find(list, field, value)
  for _, item in ipairs(list or {}) do if item[field] == value then return item end end
  fail('not_found', 'The selected item is no longer available. Refresh the view.')
end
local function active(worker)
  return worker.state == 'starting' or worker.state == 'running' or worker.state == 'exited'
end
local function worker_detail(worker)
  local detail = worker.state or 'Unknown'
  if type(worker.status) == 'table' then
    detail = worker.status.state or detail
    if type(worker.status.msg) == 'string' and worker.status.msg ~= '' then detail = detail .. ' · ' .. worker.status.msg end
  end
  if type(worker.error) == 'string' and worker.error ~= '' then detail = detail .. ' · ' .. worker.error end
  return detail
end
local function render()
  local profiles_ok, profile_result = pcall(hub, 'hub.profiles')
  local profiles = profiles_ok and (profile_result.profiles or {}) or {}
  local workers = hub('hub.workers').workers or {}
  local plugins = hub('hub.plugins').plugins or {}
  local agents = hub('hub.agents').agents or {}
  local has_messages = false
  for _, plugin in ipairs(plugins) do if plugin.id == 'messaging' then has_messages = true end end
  local nodes, plugin_items, agent_items, worker_items, stop_options = {}, {}, {}, {}, {}
  for _, plugin in ipairs(plugins) do
    if plugin.id ~= 'lazyrex' and (plugin.panel or plugin.tab) then
      plugin_items[#plugin_items + 1] = {id = plugin.id, label = plugin.name or plugin.id, action = action('open', 'Open', navigation)}
    end
  end
  if navigation_notice then nodes[#nodes + 1] = {id = 'navigation_notice', type = 'widget', text = navigation_notice} end
  nodes[#nodes + 1] = {id = 'plugins', type = 'list', label = 'Plugins', items = plugin_items}
  for _, agent in ipairs(agents) do
    if agent.id ~= 'user' then
      agent_items[#agent_items + 1] = {id = agent.id, label = agent.name or agent.id,
        detail = agent.joined and (agent.ready and 'Ready' or 'Connected') or 'Not connected',
        action = has_messages and action('message', 'Message', navigation) or nil}
    end
  end
  nodes[#nodes + 1] = {id = 'agents', type = 'list', label = 'Agents', items = agent_items}
  for _, worker in ipairs(workers) do
    local label = worker.title or worker.agent_id or worker.spawn_id
    local item_label = worker.state == 'failed' and (label .. ' · Dismiss') or label
    worker_items[#worker_items + 1] = {id = worker.spawn_id, label = item_label, detail = worker_detail(worker),
      action = worker.state == 'failed' and action('dismiss', 'Dismiss', {tool = 'hub.stop'})
        or worker.window_id and worker.state ~= 'closed' and action('open', 'Open', navigation) or nil}
    if active(worker) then stop_options[#stop_options + 1] = {value = worker.spawn_id, label = label} end
  end
  nodes[#nodes + 1] = {id = 'workers', type = 'list', label = 'Workers and accessories', items = worker_items}
  for _, profile in ipairs(profiles) do
    local fields = {{id = 'title', label = 'Tab title (optional)', kind = 'text', required = false, value = ''}}
    for _, arg in ipairs(profile.args or {}) do
      fields[#fields + 1] = {id = 'arg/' .. arg.name, label = arg.label or arg.name,
        kind = arg.name == 'task' and 'multiline' or 'text', required = arg.required == true, value = arg.default or ''}
    end
    nodes[#nodes + 1] = {id = 'launch/' .. profile.id, type = 'form', label = 'Launch ' .. (profile.name or profile.id),
      fields = fields, submit_action = action('launch', 'Launch', {tool = 'hub.spawn'})}
  end
  if not profiles_ok then
    local reason = type(profile_result) == 'table' and profile_result.message or tostring(profile_result)
    nodes[#nodes + 1] = {id = 'profiles_error', type = 'widget', text = 'Profiles could not be loaded: ' .. (reason or 'Unknown error')}
  elseif #profiles == 0 then
    nodes[#nodes + 1] = {id = 'no_profiles', type = 'widget', text = 'No profiles are configured.'}
  end
  if #stop_options > 0 then
    nodes[#nodes + 1] = {id = 'stop', type = 'form', label = 'Stop a worker or accessory', fields = {
      {id = 'worker', label = 'Worker', kind = 'select', required = true, value = stop_options[1].value, options = stop_options}
    }, submit_action = action('stop', 'Stop', {tool = 'hub.stop'})}
  end
  return {title = 'LazyRex', nodes = nodes}
end
local function perform(args)
  local input = type(args.input) == 'table' and args.input or {}
  if args.node_id == 'plugins' and args.action == 'open' then
    local plugin = find(hub('hub.plugins').plugins, 'id', input.item_id)
    if plugin.id == 'lazyrex' or not (plugin.panel or plugin.tab) then fail('invalid_input', 'This plugin has no landing view') end
    if plugin.panel then return {panel = {plugin = plugin.id, command = plugin.panel}} end
    -- A navigate effect: the hub's view worker navigates and records success or failure on this
    -- operation. hub.ui.navigate from here would answer "pending" for a lazy tab and lose the outcome.
    return {navigate = {plugin = plugin.id, tab = plugin.tab}}
  end
  if args.node_id == 'agents' and args.action == 'message' then
    local agent = find(hub('hub.agents').agents, 'id', input.item_id)
    if agent.id == 'user' then fail('invalid_input', 'Select an agent') end
    return {navigate = {plugin = 'messaging', tab = 'thread/' .. agent.id}}
  end
  if args.node_id == 'workers' and args.action == 'open' then
    local worker = find(hub('hub.workers').workers, 'spawn_id', input.item_id)
    return hub('hub.navigate.worker', {spawn_id = worker.spawn_id})
  end
  if type(args.operation_key) ~= 'string' or args.operation_key == '' then fail('invalid_input', 'An operation key is required') end
  if args.node_id == 'workers' and args.action == 'dismiss' then
    if type(input.item_id) ~= 'string' or input.item_id == '' then fail('invalid_input', 'Select a worker') end
    return hub('hub.stop', {spawn_id = input.item_id, key = args.operation_key, ['for'] = 'user'})
  end
  local values = type(input.values) == 'table' and input.values or {}
  if type(args.node_id) == 'string' and string.sub(args.node_id, 1, 7) == 'launch/' and args.action == 'launch' then
    local profile = string.sub(args.node_id, 8)
    if profile == '' then fail('invalid_input', 'Select a profile') end
    if type(values.title) ~= 'string' then fail('invalid_input', 'The title must be text') end
    local request = {profile = profile, key = args.operation_key, ['for'] = 'user'}
    local named_args = {}
    for name, value in pairs(values) do
      if string.sub(name, 1, 4) == 'arg/' then
        if type(value) ~= 'string' then fail('invalid_input', 'Launch inputs must be text') end
        named_args[string.sub(name, 5)] = value
      end
    end
    if next(named_args) then request.args = named_args end
    if values.title ~= '' then request.title = values.title end
    local result = hub('hub.spawn', request)
    navigation_notice = nil
    if result.status == 'open' and type(result.spawn_id) == 'string' then
      local shown = pcall(hub, 'hub.navigate.worker', {spawn_id = result.spawn_id})
      if not shown then navigation_notice = 'The worker started. Select the worker in the list to show its tab.' end
    end
    return result
  end
  if args.node_id == 'stop' and args.action == 'stop' then
    -- The hub owns stop retries, including a retry after the worker disappears.
    if type(values.worker) ~= 'string' or values.worker == '' then fail('invalid_input', 'Select a worker') end
    return hub('hub.stop', {spawn_id = values.worker, key = args.operation_key, ['for'] = 'user'})
  end
  fail('invalid_input', 'Unknown home action')
end
local handlers = {
  ['view.render'] = function(args) return {tree = render()} end,
  ['view.action'] = perform,
  ['view.seen'] = function(args) return {} end,
  ['hub.changed'] = function(args) return {} end,
}
local actions = {}
for name in pairs(handlers) do actions[#actions + 1] = {name = name, kind = 'client', title = name,
  args = {type = 'object', additionalProperties = true}} end
rex.call('client.keymap', {actions = actions})
hub('hub.register', {plugin = 'lazyrex', instance_id = instance_id, exports = {}})
hub('hub.ui.tabs', {tabs = {{key = 'home', title = 'LazyRex', route = '/'}}})
rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if not ev.requester or ev.requester.client_id ~= hub_id then fail('refused', 'Only the hub calls home actions') end
    local handler = handlers[ev.action]
    if not handler then fail('not_supported', 'Unknown home action') end
    local args = ev.args or {}
    if ev.action ~= 'hub.changed' and (args.route ~= '/' or args.user ~= 'user') then fail('refused', 'Unknown home route or user') end
    return handler(args)
  end)
  if ok then rex.call('client.reply', {call_id = ev.call_id, result = result})
  else rex.call('client.reply', {call_id = ev.call_id, error = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}}) end
end)
rex.call('client.set_label', {label = 'lazyrex-plugin:lazyrex'})

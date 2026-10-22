-- LazyRex home. The hub owns profiles, workers, and navigation. Home has no tab: its palette
-- entries are "Agents…" (a panel), "Stop Worker…" and, while no Profiles plugin is registered,
-- "Launch Profile…" (forms). A view at '/' is the retired home tab; it renders the panel until the
-- hub retires it.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local navigation_notice
local function fail(code, message) error({code = code, message = message}) end
-- A hub error keeps its code. Only an error without a code, or with a transient code, becomes
-- 'unavailable' (retryable): a definite refusal must end an operation, not repeat it.
local transient = {unavailable = true, disconnected = true, timeout = true}
-- hub_error turns a Rex perform error into code, message. Rex Lua gives it as text that ends in
-- "(status=<n> code=<code>)" (docs/rex-protocol.md).
local function hub_error(call_error)
  local code, message
  if type(call_error) == 'table' then
    code, message = call_error.code, call_error.message
  else
    local text = tostring(call_error or 'Hub request failed')
    code = text:match('code=([%w_%-%.]+)%)%s*$')
    message = text:match('failed: (.*) %(status=%d+ code=[^)]*%)%s*$') or text
    -- A hub error message keeps its own "code: " prefix; drop it when it repeats the code.
    if code and message:sub(1, #code + 2) == code .. ': ' then message = message:sub(#code + 3) end
  end
  if not code or transient[code] then code = 'unavailable' end
  return code, message or 'Hub request failed'
end
local function hub(action, args)
  local response, call_error = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if type(response) ~= 'table' then fail(hub_error(call_error)) end
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
local function cancel_node(label)
  return {id = 'cancel', type = 'widget', text = '', action = {name = '$cancel', label = label or 'Cancel', enabled = true}}
end
local function find(list, field, value)
  for _, item in ipairs(list or {}) do if item[field] == value then return item end end
  fail('not_found', 'The selected item is no longer available. Refresh the view.')
end
local function active(worker)
  return worker.state == 'starting' or worker.state == 'running' or worker.state == 'exited'
end
local function worker_label(worker) return worker.title or worker.agent_id or worker.spawn_id end
local function worker_detail(worker)
  local detail = worker.state or 'Unknown'
  if type(worker.status) == 'table' then
    detail = worker.status.state or detail
    if type(worker.status.msg) == 'string' and worker.status.msg ~= '' then detail = detail .. ' · ' .. worker.status.msg end
  end
  if type(worker.error) == 'string' and worker.error ~= '' then detail = detail .. ' · ' .. worker.error end
  return detail
end
local function registered(id)
  for _, plugin in ipairs(hub('hub.plugins').plugins or {}) do if plugin.id == id then return true end end
  return false
end

-- /panels/agents: what runs and what the user can talk to. One action per list item.
local function panel_tree()
  local workers = hub('hub.workers').workers or {}
  local agents = hub('hub.agents').agents or {}
  local has_messages = registered('messaging')
  local nodes, agent_items, worker_items = {}, {}, {}
  if navigation_notice then nodes[#nodes + 1] = {id = 'navigation_notice', type = 'widget', text = navigation_notice} end
  for _, agent in ipairs(agents) do
    if agent.id ~= 'user' then
      agent_items[#agent_items + 1] = {id = agent.id, label = agent.name or agent.id,
        detail = agent.joined and (agent.ready and 'Ready' or 'Connected') or 'Not connected',
        action = has_messages and action('message', 'Message', navigation) or nil}
    end
  end
  nodes[#nodes + 1] = {id = 'agents', type = 'list', label = 'Agents', items = agent_items}
  for _, worker in ipairs(workers) do
    local label = worker_label(worker)
    local item_action
    if worker.state == 'failed' then
      label, item_action = label .. ' · Dismiss', action('dismiss', 'Dismiss', {tool = 'hub.stop'})
    elseif active(worker) then
      item_action = action('manage', 'Manage…', navigation)
    elseif worker.window_id and worker.state ~= 'closed' then
      item_action = action('open', 'Open', navigation)
    end
    worker_items[#worker_items + 1] = {id = worker.spawn_id, label = label, detail = worker_detail(worker), action = item_action}
  end
  nodes[#nodes + 1] = {id = 'workers', type = 'list', label = 'Workers and accessories', items = worker_items}
  return {title = 'Agents', nodes = nodes}
end

-- /forms/worker/<spawn_id>: the menu of one active worker.
local function worker_tree(spawn_id)
  local worker
  for _, w in ipairs(hub('hub.workers').workers or {}) do if w.spawn_id == spawn_id then worker = w end end
  if not worker then
    return {title = 'Worker', menu = true, nodes = {
      {id = 'gone', type = 'widget', text = 'This worker is no longer listed.'}, cancel_node('Close')}}
  end
  local nodes = {{id = 'detail', type = 'widget', text = worker_detail(worker)}}
  if worker.window_id and worker.state ~= 'closed' then
    nodes[#nodes + 1] = {id = 'open', type = 'widget', text = '', action = action('open', 'Open', navigation)}
  end
  if active(worker) then
    nodes[#nodes + 1] = {id = 'stop', type = 'widget', text = '', action = action('stop', 'Stop', {tool = 'hub.stop'})}
  end
  nodes[#nodes + 1] = cancel_node('Close')
  return {title = worker_label(worker), menu = true, nodes = nodes}
end

-- /forms/stop: the keyboard path, a select of the active workers.
local function stop_tree()
  local options = {}
  for _, worker in ipairs(hub('hub.workers').workers or {}) do
    if active(worker) then options[#options + 1] = {value = worker.spawn_id, label = worker_label(worker)} end
  end
  local nodes = {}
  if #options == 0 then
    nodes[1] = {id = 'no_workers', type = 'widget', text = 'No worker or accessory runs.'}
  else
    nodes[1] = {id = 'stop', type = 'form', label = 'Stop a worker or accessory', fields = {
      {id = 'worker', label = 'Worker', kind = 'select', required = true, value = options[1].value, options = options}
    }, submit_action = action('stop', 'Stop', {tool = 'hub.stop'})}
  end
  nodes[#nodes + 1] = cancel_node()
  return {title = 'Stop Worker', nodes = nodes}
end

-- /forms/launch: one launch form per profile (registered only while no Profiles plugin is).
local function launch_tree()
  local profiles_ok, profile_result = pcall(hub, 'hub.profiles')
  local profiles = profiles_ok and (profile_result.profiles or {}) or {}
  local nodes = {}
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
  nodes[#nodes + 1] = cancel_node()
  return {title = 'Launch Profile', nodes = nodes}
end

local function render(route)
  if route == '/panels/agents' or route == '/' then return panel_tree() end
  if route == '/forms/stop' then return stop_tree() end
  if route == '/forms/launch' then return launch_tree() end
  return worker_tree(route:match('^/forms/worker/(.+)$'))
end

local function stop(spawn_id, key)
  -- The hub owns stop retries, including a retry after the worker disappears.
  return hub('hub.stop', {spawn_id = spawn_id, key = key, ['for'] = 'user'})
end
local function closing(result)
  result.close_form = true
  return result
end
local function perform(args)
  local input = type(args.input) == 'table' and args.input or {}
  if args.node_id == 'agents' and args.action == 'message' then
    local agent = find(hub('hub.agents').agents, 'id', input.item_id)
    if agent.id == 'user' then fail('invalid_input', 'Select an agent') end
    return {navigate = {plugin = 'messaging', tab = 'thread/' .. agent.id}}
  end
  if args.node_id == 'workers' and (args.action == 'open' or args.action == 'manage') then
    local worker = find(hub('hub.workers').workers, 'spawn_id', input.item_id)
    if args.action == 'manage' then return {form = '/forms/worker/' .. worker.spawn_id} end
    return hub('hub.navigate.worker', {spawn_id = worker.spawn_id})
  end
  local menu_id = args.route:match('^/forms/worker/(.+)$')
  if menu_id and args.node_id == 'open' and args.action == 'open' then
    local worker = find(hub('hub.workers').workers, 'spawn_id', menu_id)
    return closing(hub('hub.navigate.worker', {spawn_id = worker.spawn_id}))
  end
  if type(args.operation_key) ~= 'string' or args.operation_key == '' then fail('invalid_input', 'An operation key is required') end
  if args.node_id == 'workers' and args.action == 'dismiss' then
    if type(input.item_id) ~= 'string' or input.item_id == '' then fail('invalid_input', 'Select a worker') end
    return stop(input.item_id, args.operation_key)
  end
  if menu_id and args.node_id == 'stop' and args.action == 'stop' then
    return closing(stop(menu_id, args.operation_key))
  end
  local values = type(input.values) == 'table' and input.values or {}
  if args.route == '/forms/launch' and type(args.node_id) == 'string' and string.sub(args.node_id, 1, 7) == 'launch/' and args.action == 'launch' then
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
      if not shown then navigation_notice = 'The worker started. Select the worker in Agents… to show its tab.' end
      return closing(result)
    end
    return result -- a failed launch keeps the form, with its values
  end
  if args.route == '/forms/stop' and args.node_id == 'stop' and args.action == 'stop' then
    if type(values.worker) ~= 'string' or values.worker == '' then fail('invalid_input', 'Select a worker') end
    return closing(stop(values.worker, args.operation_key))
  end
  fail('invalid_input', 'Unknown home action')
end

-- The palette entries. "Launch Profile…" only while no Profiles plugin is registered: Profiles
-- has its own Start Agent… and Start Accessory… (decision Q1 A, 2026-10-22).
local function commands()
  local list = {
    {id = 'agents', title = 'Agents…', route = '/panels/agents', surface = 'panel', landing = true, user_only = 'navigation'},
    {id = 'stop', title = 'Stop Worker…', route = '/forms/stop', action = 'hub.stop'},
  }
  if not registered('profiles') then
    list[#list + 1] = {id = 'launch', title = 'Launch Profile…', route = '/forms/launch', action = 'hub.spawn'}
  end
  return list
end
-- sync registers again only when the command set changed. A registration makes the hub send
-- hub.changed (plugins) again, which then finds no change: no loop. Each call reads the current
-- plugin list, so the last of a burst of changes wins.
local registered_ids
local function sync()
  local list = commands()
  local ids = {}
  for _, c in ipairs(list) do ids[#ids + 1] = c.id end
  ids = table.concat(ids, ',')
  if ids == registered_ids then return end
  hub('hub.register', {plugin = 'lazyrex', instance_id = instance_id, exports = {}, commands = list})
  registered_ids = ids
end

local routes = {['/'] = true, ['/panels/agents'] = true, ['/forms/stop'] = true, ['/forms/launch'] = true}
local function known_route(route)
  return type(route) == 'string' and (routes[route] or route:match('^/forms/worker/.+$') ~= nil)
end
local handlers = {
  ['view.render'] = function(args) return {tree = render(args.route)} end,
  ['view.action'] = perform,
  ['view.seen'] = function(args) return {} end,
  ['hub.changed'] = function(args) sync(); return {} end,
}
local actions = {}
for name in pairs(handlers) do actions[#actions + 1] = {name = name, kind = 'client', title = name,
  args = {type = 'object', additionalProperties = true}} end
rex.call('client.keymap', {actions = actions})
-- The first registration names the base commands, so the hub knows the plugin; sync then adds
-- Launch Profile… when no Profiles plugin is registered.
hub('hub.register', {plugin = 'lazyrex', instance_id = instance_id, exports = {}, commands = {
  {id = 'agents', title = 'Agents…', route = '/panels/agents', surface = 'panel', landing = true, user_only = 'navigation'},
  {id = 'stop', title = 'Stop Worker…', route = '/forms/stop', action = 'hub.stop'}}})
registered_ids = 'agents,stop'
sync()
hub('hub.ui.tabs', {tabs = {}})
rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if not ev.requester or ev.requester.client_id ~= hub_id then fail('refused', 'Only the hub calls home actions') end
    local handler = handlers[ev.action]
    if not handler then fail('not_supported', 'Unknown home action') end
    local args = ev.args or {}
    if ev.action ~= 'hub.changed' and (not known_route(args.route) or args.user ~= 'user') then fail('refused', 'Unknown home route or user') end
    return handler(args)
  end)
  if ok then rex.call('client.reply', {call_id = ev.call_id, result = result})
  else rex.call('client.reply', {call_id = ev.call_id, error = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}}) end
end)
rex.call('client.set_label', {label = 'lazyrex-plugin:lazyrex'})

-- An isolation probe plugin (docs/testing.md, "Isolation"). The manifest loads it twice: as the
-- plugin under test (isofix) and as a dependency (isodep, through isodep/probe.lua). Its plugin
-- args name it. At load it writes what it sees to $HOME/<name>-env.json, and it writes <name>-cwd
-- with a relative path.
-- Its home view has a form whose tool is a hub action (hub.spawn), for the parity sweep.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local name = assert(rex.args.name, 'the plugin arg name is required')

local function hub(action, args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if err or not res then error({code = 'hub_failed', message = tostring(err)}) end
  return res.result
end

local function exists(path)
  local f = io.open(path, 'r')
  if f then f:close() end
  return f ~= nil
end

local home = os.getenv('HOME') or ''
local seen = {
  HOME = os.getenv('HOME'), XDG_CONFIG_HOME = os.getenv('XDG_CONFIG_HOME'), XDG_DATA_HOME = os.getenv('XDG_DATA_HOME'),
  XDG_STATE_HOME = os.getenv('XDG_STATE_HOME'), XDG_CACHE_HOME = os.getenv('XDG_CACHE_HOME'), TMPDIR = os.getenv('TMPDIR'),
  REX_SERVER = os.getenv('REX_SERVER'), secret = os.getenv('LRT_OUTER_SECRET'), plain = os.getenv('LRT_PLAIN'),
  data = os.getenv('LRT_DATA'), marker_dir = rex.args.marker_dir,
  home_sentinel = exists(home .. '/.lrt-sentinel'),
  config_sentinel = exists((os.getenv('XDG_CONFIG_HOME') or '') .. '/.lrt-sentinel'),
}
local f = assert(io.open(home .. '/' .. name .. '-env.json', 'w'))
f:write(rex.json.encode(seen))
f:close()
local rel = io.open(name .. '-cwd', 'w')
if rel then rel:write('cwd'); rel:close() end

local obj = {type = 'object'}
rex.call('client.keymap', {actions = {
  {name = 'view.render', kind = 'client', title = 'Render', args = obj},
  {name = 'view.action', kind = 'client', title = 'Action', args = obj},
}})

local function home_tree()
  return {title = 'Probe ' .. name, nodes = {
    {id = 'launch', type = 'form', fields = {}, submit_action = {name = 'submit', label = 'Launch', enabled = true, tool = 'hub.spawn'}},
    {id = 'stop', type = 'widget', text = 'Stop', action = {name = 'stop', label = 'Stop', enabled = true, tool = 'hub.stop'}},
  }}
end

rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if ev.requester.client_id ~= hub_id then error({code = 'refused', message = 'Only the hub may call ' .. ev.action}) end
    if ev.action == 'view.render' then return {tree = home_tree()} end
    error({code = 'invalid_input', message = 'the probe has no actions'})
  end)
  if ok then
    rex.call('client.reply', {call_id = ev.call_id, result = result})
  else
    local e = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}
    rex.call('client.reply', {call_id = ev.call_id, error = e})
  end
end)

hub('hub.register', {plugin = name, instance_id = instance_id, exports = {}})
hub('hub.ui.tabs', {tabs = {{key = 'home', title = 'Probe ' .. name, route = '/home'}}})
rex.call('client.set_label', {label = 'lazyrex-plugin:' .. name})

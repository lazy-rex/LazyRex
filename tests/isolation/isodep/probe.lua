-- The isolation probe of the dependency isodep (docs/testing.md, "Isolation"). A local plugin reads
-- only its own directory (docs/plugin-sources.md, section 2), so isodep does not load ../probe.lua.
-- At load it writes what it sees to $HOME/<name>-env.json with the same fields as ../probe.lua, and
-- it writes <name>-cwd with a relative path. It has no views and no actions.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local name = assert(rex.args.name, 'the plugin arg name is required')

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

rex.on('client_perform_requested', function(ctx, ev)
  rex.call('client.reply', {call_id = ev.call_id, error = {code = 'invalid_input', message = 'isodep has no actions'}})
end)

local res, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.register',
  args = {plugin = name, instance_id = instance_id, exports = {}}, timeout_ms = 10000})
if err or not res then error({code = 'hub_failed', message = tostring(err)}) end
rex.call('client.set_label', {label = 'lazyrex-plugin:' .. name})

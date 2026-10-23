-- Parity fixture (docs/testing.md, "Agent parity"): renders the home forms that the sweep does not
-- open itself. "Launch Profile…" is a palette command only while no profiles plugin is registered
-- (the stub is registered at start), and a worker menu opens only from the Agents… panel.
local function commands()
  local ids = {}
  for _, c in ipairs(H.hub('hub.test.commands', {plugin = 'lazyrex'}).commands or {}) do ids[c.id] = true end
  return ids
end
H.hub('hub.admin.stop', {plugin = 'launcher'})
wait('the home plugin registers Launch Profile…', function() return commands().launch end)
user:command('launch')
user:view('/forms/launch'):render()
H.hub('hub.admin.start', {plugin = 'launcher'})
wait('the home plugin drops Launch Profile…', function() return not commands().launch end)

local w = agent('codex', {kind = 'codex', title = 'Codex'}):call('hub.spawn', {profile = 'shell', key = 'parity-worker', title = 'Parity worker'})
user:command('agents')
local panel = user:view('/panels/agents')
panel:render()
local op = panel:action('workers', 'manage', {item_id = w.spawn_id})
if op.status ~= 'succeeded' or op.effects == 'failed' then error('the worker menu did not open: ' .. show(op), 0) end
user:view('/forms/worker/' .. w.spawn_id):render()

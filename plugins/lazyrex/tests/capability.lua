-- The capability decides, not the name: in this file the launcher stub registers with no command
-- (test.files args commands = none). A registered plugin that offers no hub.spawn command leaves
-- "Launch Profile…" in place.

local function ids()
  local out = {}
  for _, c in ipairs(H.hub('hub.test.commands', {plugin = 'lazyrex'}).commands or {}) do out[#out + 1] = c.id end
  return table.concat(out, ',')
end

test('a registered plugin without a hub.spawn command: Launch Profile… stays', function()
  expect(H.hub('hub.test.status', {}).plugins.launcher.registered).to_be(true)
  wait('Launch Profile… is registered', function() return ids() == 'agents,stop,plugins,launch' end)
end)

test('hub.plugins reports each plugin\'s palette commands', function()
  local found
  for _, p in ipairs(H.hub('hub.plugins', {}).plugins or {}) do
    if p.id == 'messaging' then found = p end
  end
  expect(found.commands).to_equal({{id = 'threads', title = 'Threads', route = '/panels/threads', surface = 'panel', landing = true, user_only = 'navigation'}})
end)

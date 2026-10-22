-- The palette entries of the home plugin follow the Profiles plugin: "Launch Profile…" is
-- registered only while no plugin named profiles is registered (decision Q1 A, 2026-10-22). The
-- profiles stub stands in for Profiles; hub.admin.stop and hub.admin.start change its state.

local function ids()
  local out = {}
  for _, c in ipairs(H.hub('hub.test.commands', {plugin = 'lazyrex'}).commands or {}) do out[#out + 1] = c.id end
  return table.concat(out, ',')
end
local function registered(name)
  return H.hub('hub.test.status', {}).plugins[name].registered
end
local function registrations()
  local n = 0
  for _, e in ipairs(H.hub('hub.test.events', {event = 'plugin_registered'}).events or {}) do
    if e.plugin == 'lazyrex' then n = n + 1 end
  end
  return n
end

test('with Profiles registered: no Launch Profile…', function()
  expect(ids()).to_be('agents,stop')
end)

test('Profiles stops: Launch Profile… is registered; Profiles starts: it goes again', function()
  H.hub('hub.admin.stop', {plugin = 'profiles'})
  wait('Launch Profile… is registered', function() return ids() == 'agents,stop,launch' end)
  H.hub('hub.admin.start', {plugin = 'profiles'})
  wait('Launch Profile… goes', function() return ids() == 'agents,stop' end)
end)

test('a burst of changes gives one final command set, the latest; no registration loop', function()
  H.hub('hub.admin.stop', {plugin = 'profiles'})
  H.hub('hub.admin.start', {plugin = 'profiles'})
  H.hub('hub.admin.stop', {plugin = 'profiles'})
  wait('profiles is stopped', function() return not registered('profiles') end)
  wait('the latest set: with Launch Profile…', function() return ids() == 'agents,stop,launch' end)
  -- The set stays, and the home plugin stops registering (a registration sends hub.changed again).
  local n = registrations()
  rex.sleep(3)
  expect(ids()).to_be('agents,stop,launch')
  expect(registrations(), 'no further registration').to_be(n)
  H.hub('hub.admin.start', {plugin = 'profiles'})
  H.hub('hub.admin.stop', {plugin = 'profiles'})
  H.hub('hub.admin.start', {plugin = 'profiles'})
  wait('profiles is registered', function() return registered('profiles') end)
  wait('the latest set: without Launch Profile…', function() return ids() == 'agents,stop' end)
  rex.sleep(3)
  expect(ids()).to_be('agents,stop')
end)

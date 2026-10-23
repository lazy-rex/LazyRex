-- hub.ui.navigate to a tab that the plugin declares on view.open (a lazy tab). The plugin's
-- declaration calls the hub during view.open, so a hub handler that waits for view.open stalls
-- for the full call timeout (rex-protocol.md, "The hub has one request queue").

-- opens counts the fixture's view.open calls (its table opens).
local function opens() return tonumber(sql('SELECT count(*) AS n FROM opens')[1].n) end

-- committed returns the tabs of the committed navigations, oldest first.
local function committed()
  local out = {}
  for _, e in ipairs(H.hub('hub.test.events', {event = 'navigation_committed'}).events or {}) do out[#out + 1] = e.tab end
  return out
end

local function navigate(tab)
  local started = os.time()
  local ok, r = pcall(H.hub, 'hub.ui.navigate', {plugin = 'harnessfix', tab = tab})
  return ok, r, os.time() - started
end

-- poll asks for the outcome of a pending navigation by its ID.
local function poll(pending)
  return wait('navigation ' .. tostring(pending.navigation_id), function()
    local ok, r = pcall(H.hub, 'hub.ui.navigate', {navigation_id = pending.navigation_id})
    if not ok then return {error = r} end
    if r.status == 'pending' then return nil end
    return r
  end, 15)
end

test('an undeclared tab answers at once and opens', function()
  local ok, r, took = navigate('lazy/a')
  expect(took <= 2, 'answer time in seconds ' .. took).to_be(true)
  expect(ok, tostring(r)).to_be(true)
  local done = r.status == 'pending' and poll(r) or r
  expect(done.status).to_be('accepted')
  expect(tab('lazy/a')).to_be_truthy()
  expect(node(user:view('lazy/a'):render(), 'lazy').text).to_be('/lazy/a')
end)

test('a tab that view.open does not declare fails, and polling does not open again', function()
  local before = opens()
  local ok, r = navigate('nope/x')
  expect(ok, tostring(r)).to_be(true)
  expect(r.status).to_be('pending')
  expect(H.hub('hub.ui.navigate', {navigation_id = r.navigation_id}).status).to_be('pending')
  local done = poll(r)
  expect(done.error and tostring(done.error) or '').to_match('has no tab')
  expect(opens()).to_be(before + 1)
end)

test('an older navigation that finishes late does not replace a newer one', function()
  local ok, r = navigate('lazy/slow')
  expect(ok, tostring(r)).to_be(true)
  expect(r.status).to_be('pending')
  expect(H.hub('hub.ui.navigate', {plugin = 'harnessfix', tab = 'home'}).status).to_be('accepted')
  local late = poll(r)
  expect(late.status).to_be('superseded')
  local navs = committed()
  expect(navs[#navs]).to_be('home')
  expect(navs).not_to_contain('lazy/slow')
  expect(tab('lazy/slow')).to_be_truthy()
end)

test('a new navigation to a tab is not answered by an older one that nobody polled', function()
  local ok, r = navigate('lazy/b')
  expect(ok, tostring(r)).to_be(true)
  wait('lazy/b to be declared', function() return tab('lazy/b') end)
  expect(H.hub('hub.ui.navigate', {plugin = 'harnessfix', tab = 'home'}).status).to_be('accepted')
  expect(H.hub('hub.ui.navigate', {plugin = 'harnessfix', tab = 'lazy/b'}).status).to_be('accepted')
  local navs = committed()
  expect(navs[#navs - 1]).to_be('home')
  expect(navs[#navs]).to_be('lazy/b')
end)

test('an unknown navigation ID is refused', function()
  expect_error(function() H.hub('hub.ui.navigate', {navigation_id = 'nav-999999'}) end, 'not_found')
end)

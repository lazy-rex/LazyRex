-- hub.ui.navigate to a tab that the plugin declares on view.open (a lazy tab). The plugin's
-- declaration calls the hub during view.open, so a hub handler that waits for view.open stalls
-- for the full call timeout (rex-protocol.md, "The hub has one request queue").

local function navigate(tab)
  local started = os.time()
  local ok, r = pcall(H.hub, 'hub.ui.navigate', {plugin = 'harnessfix', tab = tab})
  return ok, r, os.time() - started
end

-- poll asks again with the same arguments until the answer is not pending.
local function poll(tab)
  return wait('navigation to ' .. tab, function()
    local ok, r = pcall(H.hub, 'hub.ui.navigate', {plugin = 'harnessfix', tab = tab})
    if not ok then return {error = r} end
    if r.status == 'pending' then return nil end
    return r
  end, 15)
end

test('an undeclared tab answers at once and opens', function()
  local ok, r, took = navigate('lazy/a')
  expect(took <= 2, 'answer time in seconds ' .. took).to_be(true)
  expect(ok, tostring(r)).to_be(true)
  local done = r.status == 'pending' and poll('lazy/a') or r
  expect(done.status).to_be('accepted')
  expect(tab('lazy/a')).to_be_truthy()
  expect(node(user:view('lazy/a'):render(), 'lazy').text).to_be('/lazy/a')
end)

test('a tab that view.open does not declare fails, and polling does not open again', function()
  local before = store('opens').value or 0
  local ok, r = navigate('nope/x')
  expect(ok, tostring(r)).to_be(true)
  expect(r.status).to_be('pending')
  expect(H.hub('hub.ui.navigate', {plugin = 'harnessfix', tab = 'nope/x'}).status).to_be('pending')
  local done = poll('nope/x')
  expect(done.error and tostring(done.error) or '').to_match('has no tab')
  expect(store('opens').value).to_be(before + 1)
end)

test('an older navigation that finishes late does not replace a newer one', function()
  local ok, r = navigate('lazy/slow')
  expect(ok, tostring(r)).to_be(true)
  expect(r.status).to_be('pending')
  expect(H.hub('hub.ui.navigate', {plugin = 'harnessfix', tab = 'home'}).status).to_be('accepted')
  local late = poll('lazy/slow')
  expect(late.status).to_be('superseded')
  expect(H.hub('hub.test.status', {}).nav).to_be('harnessfix/home')
  expect(tab('lazy/slow')).to_be_truthy()
end)

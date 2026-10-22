-- The home plugin may declare no tabs (the home tab became a panel). Its open tab view then retires
-- through the legacy gate (view.retire): the renderer leaves when nothing is unsaved, and the hub
-- never closes it as undeclared. This file runs with the fixture as the hub's home plugin
-- (lazyrex.json test.files home_retire.lua home = harnessfix).

local hold = H.args.lab .. '/testview-hold'
local function set_hold(route, mode)
  local f = assert(io.open(hold, 'w'))
  f:write(route .. '\n' .. mode .. '\n')
  f:close()
end

local function open_view(id)
  for _, w in ipairs(views()) do
    if w.view_id == id then return w end
  end
end

local function events(name, id)
  local out = {}
  for _, e in ipairs(H.hub('hub.test.events', {event = name}).events or {}) do
    if e.view_id == id then out[#out + 1] = e end
  end
  return out
end

local function declare_none()
  local r = agent('alice'):call('fix.hub', {action = 'hub.ui.tabs', args = {tabs = {}}})
  expect(r.error).to_be(nil)
  expect(tabs()).to_equal({})
end

-- The fixture declares its tab again (fix.title declares its whole list).
local function declare_home()
  agent('alice'):direct('fix.title', {key = 'home', title = 'Fixture home'})
end

test('the home plugin may declare no tabs; its open view retires', function()
  local v = user:open('home')
  declare_none()
  wait('the home view to retire', function() return open_view(v.id) == nil end)
  expect(#events('view_retire', v.id) >= 1, 'the hub asked view.retire').to_be(true)
  for _, e in ipairs(events('view_closed', v.id)) do
    expect(e.why ~= 'undeclared', 'not closed as undeclared').to_be(true)
  end
  declare_home()
  user:open('home'):render()
end)

test('a home view with unsaved text waits; it retires once the text is saved', function()
  local v = user:open('home')
  set_hold('/home', 'retire-wait')
  declare_none()
  wait('the hub to ask view.retire', function() return #events('view_retire', v.id) >= 1 end)
  -- Passes run (the hub retries a running retirement): the view stays, with its pane.
  rex.sleep(2)
  expect(open_view(v.id) ~= nil, 'the waiting view stays open').to_be(true)
  expect(#events('view_closed', v.id)).to_be(0)
  os.remove(hold)
  wait('the home view to retire', function() return open_view(v.id) == nil end)
  declare_home()
  user:open('home'):render()
end)

test('a hub restart during the retirement resumes it and completes it once', function()
  local v = user:open('home')
  set_hold('/home', 'retire-wait')
  declare_none()
  wait('the hub to ask view.retire', function() return #events('view_retire', v.id) >= 1 end)
  hub:restart()
  wait('the view to attach again', function()
    local w = open_view(v.id)
    return w and w.attached
  end, 20)
  -- The new hub asks again: the missing declaration is durable.
  wait('the new hub to ask view.retire', function() return #events('view_retire', v.id) >= 1 end, 20)
  expect(open_view(v.id) ~= nil, 'the waiting view stays open').to_be(true)
  os.remove(hold)
  wait('the home view to retire', function() return open_view(v.id) == nil end)
  local closed = events('view_closed', v.id)
  expect(#closed <= 1, 'one close at most').to_be(true)
  declare_home()
  user:open('home'):render()
end)

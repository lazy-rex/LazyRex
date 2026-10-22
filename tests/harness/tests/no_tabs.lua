-- A plugin that declares no tabs (hub.ui.tabs {}) still gets a reconciliation pass: its open tab
-- view closes as undeclared, not stranded (the plugin has no ui_tabs row, but a live view row).

local function open_view(id)
  for _, w in ipairs(views()) do
    if w.view_id == id then return w end
  end
end

test('a non-home plugin that declares no tabs closes its open tab view', function()
  local v = user:open('home')
  expect(open_view(v.id) ~= nil, 'the fixture home view is open').to_be(true)
  local r = agent('alice'):call('fix.hub', {action = 'hub.ui.tabs', args = {tabs = {}}})
  expect(r.error).to_be(nil)
  wait('the undeclared view to close', function() return open_view(v.id) == nil end)
  expect(tabs()).to_equal({})
  -- The fixture declares its tab again (fix.title declares its whole list); a new view opens.
  agent('alice'):direct('fix.title', {key = 'home', title = 'Fixture home'})
  user:open('home'):render()
end)

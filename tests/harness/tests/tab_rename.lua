-- Tab rename (hub/cmd/hub/tabrename.go): the user renames a plugin view's Rex tab, and the plugin
-- gets view.rename. The fixture keeps the title literally and declares it again; the record
-- renames lists every title it got. A label that the hub writes (from a declared title) is never a
-- rename. It replaces the Node lab tab-rename-lab.mjs (Messages thread tabs).

local alice = agent('alice')

local function renames() return store('renames').value or {} end
local function events() return #(H.hub('hub.test.events', {event = 'tab_rename'}).events or {}) end
local function title() return H.tab('home').title end

local function home()
  local v = user:open('home')
  v:render()
  wait('the tab shows the declared title', function() return v:tab_label() == title() end)
  return v
end

local function rename(v, label, want)
  v:rename_tab(label)
  wait('the title ' .. (want or label), function() return title() == (want or label) end)
end

test('a label that the hub writes is not a rename', function()
  local v = home()
  local before = #renames()
  alice:direct('fix.title', {key = 'home', title = 'Fixture home (3)'})
  wait('the hub writes the new label', function() return v:tab_label() == 'Fixture home (3)' end)
  rex.sleep(1)
  expect(#renames(), 'no rename').to_be(before)
end)

test('a user rename reaches the plugin literally and the tab keeps it', function()
  local v = home()
  local before = #renames()
  rename(v, 'Review (2)')
  wait('the tab shows the title', function() return v:tab_label() == 'Review (2)' end)
  expect(renames()[before + 1]).to_be('Review (2)')
  -- The plugin adds an unread count after the user title: the hub's label is not a rename.
  alice:direct('fix.title', {key = 'home', title = 'Review (2) (1)'})
  wait('the hub writes the count label', function() return v:tab_label() == 'Review (2) (1)' end)
  rex.sleep(1)
  expect(#renames(), 'only the user rename').to_be(before + 1)
end)

test('A, B, A gives three renames', function()
  local v = home()
  local before = #renames()
  for _, t in ipairs({'Alpha', 'Beta', 'Alpha'}) do rename(v, t) end
  local got = renames()
  expect({got[before + 1], got[before + 2], got[before + 3]}).to_equal({'Alpha', 'Beta', 'Alpha'})
end)

test('rapid renames reach the plugin in order and the last one wins', function()
  local v = home()
  local before = #renames()
  for _, t in ipairs({'R1', 'R2', 'R3'}) do v:rename_tab(t) end
  wait('the title R3', function() return title() == 'R3' end)
  rex.sleep(1)
  expect(title()).to_be('R3')
  local got = renames()
  local tail = {}
  for i = before + 1, #got do tail[#tail + 1] = got[i] end
  expect(tail).to_equal({'R1', 'R2', 'R3'})
  wait('the tab shows R3', function() return v:tab_label() == 'R3' end)
end)

test('the generated label is not a rename; the declared title comes back', function()
  local v = home()
  local declared = title()
  local before, ev = #renames(), events()
  v:rename_tab('') -- Rex gives the window its generated label
  wait('the declared title comes back', function() return v:tab_label() == declared end)
  rex.sleep(1)
  expect(#renames(), 'no rename').to_be(before)
  expect(events(), 'no tab_rename event').to_be(ev)
end)

test('hub label, user title, hub label again: two renames', function()
  local v = home()
  local hub_label = v:tab_label()
  local before = #renames()
  rename(v, 'Other')
  rename(v, hub_label)
  local got = renames()
  expect({got[before + 1], got[before + 2]}).to_equal({'Other', hub_label})
end)

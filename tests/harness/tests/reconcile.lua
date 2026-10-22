-- The hub's UI reconciler (hub/cmd/hub/ui.go) with real Rex windows: a navigation that opens the
-- declared tabs, a native close, a navigation that reopens, a tab that the plugin stops declaring,
-- and a renderer crash. The fixture declares its tabs through fix.hub (hub.ui.tabs as the plugin).
-- All plugins share the LazyRex session and the hub does not order a plugin's tabs, so the tab
-- checks compare sorted labels.

local alice = agent('alice')
local PLUGIN = 'harnessfix'
local THREE = {
  {key = 'home', title = 'Fixture home', route = '/home'},
  {key = 't1', title = 'Thread 1', route = '/thread/1'},
  {key = 't2', title = 'Thread 2', route = '/thread/2'},
}

local function declare(tabs)
  local r = alice:direct('fix.hub', {action = 'hub.ui.tabs', args = {tabs = tabs}})
  expect(r.error, 'hub.ui.tabs').to_be(nil)
end

local function describe(plugin) return H.hub('hub.ui.describe', {plugin = plugin or PLUGIN}) end

local function view_of_tab(d, tab)
  for _, v in ipairs(d.views or {}) do
    if v.tab == tab then return v end
  end
  return nil
end

local function windows(session)
  local sv, err = H.rex_call('session.view', {session_id = session})
  if err then error(err, 0) end
  return sv.windows or {}
end

-- labels are the labels of the fixture's tabs in the plugin session, sorted: the fixture does not
-- order the shared session.
local function labels()
  local mine = {}
  for _, t in ipairs(THREE) do mine[t.title] = true end
  local out = {}
  for _, w in ipairs(windows(describe().session_id)) do
    if mine[w.label] then out[#out + 1] = w.label end
  end
  table.sort(out)
  return table.concat(out, ',')
end

local function wait_labels(want)
  wait('the tabs ' .. want, function() return labels() == want end)
end

test('a navigation opens every declared tab, each attached', function()
  declare(THREE)
  user:open('home')
  wait('the three views open and attached', function()
    local d = describe()
    for _, t in ipairs(THREE) do
      local v = view_of_tab(d, t.key)
      if not (v and v.status == 'open' and v.attached) then return nil end
    end
    return true
  end)
  wait_labels('Fixture home,Thread 1,Thread 2')
end)

test('a native close closes the view; the tab stays closed', function()
  local d = describe()
  local t1 = view_of_tab(d, 't1')
  local _, err = H.rex_call('session.close_window', {session_id = d.session_id, window_id = t1.window_id})
  expect(err, 'close_window').to_be(nil)
  wait('the tab t1 suppressed', function()
    for _, t in ipairs(describe().tabs or {}) do
      if t.tab == 't1' and t.suppressed then return true end
    end
    return nil
  end)
  wait_labels('Fixture home,Thread 2')
  rex.sleep(2) -- the reconciler does not open it again
  expect(view_of_tab(describe(), 't1'), 'no view of t1').to_be(nil)
  expect(labels()).to_be('Fixture home,Thread 2')
end)

test('a navigation reopens a closed tab', function()
  user:open('t1')
  wait_labels('Fixture home,Thread 1,Thread 2')
end)

test('a tab that the plugin stops declaring closes', function()
  declare({THREE[1], THREE[2]})
  wait_labels('Fixture home,Thread 1')
  wait('no view of t2', function() return view_of_tab(describe(), 't2') == nil end)
end)

test('a renderer crash: a new attached view of the same tab', function()
  local v = user:view('t1')
  local before = view_of_tab(describe(), 't1')
  v:crash()
  local after = wait('a new attached view of t1', function()
    local w = view_of_tab(describe(), 't1')
    if w and w.attached and w.block_id ~= before.block_id then return w end
    return nil
  end)
  expect(after.view_id, 'a new view').not_to_be(before.view_id)
  expect(after.status).to_be('open')
  wait_labels('Fixture home,Thread 1')
end)

-- events are the hub's events with this name for one view.
local function events(name, view_id)
  local out = {}
  for _, e in ipairs(H.hub('hub.test.events', {event = name}).events or {}) do
    if e.view_id == view_id then out[#out + 1] = e end
  end
  return out
end

-- check_close is the event record of a close from inside the view: one user close, no exit.
local function check_close(view_id, when)
  local closed = events('view_closed', view_id)
  expect(#closed, 'view_closed ' .. when).to_be(1)
  expect(closed[1].by, 'closed by ' .. when).to_be('user')
  expect(closed[1].from, 'closed from ' .. when).to_be('view')
  expect(#events('view_exited', view_id), 'view_exited (a crash) ' .. when).to_be(0)
end

test('a close from inside a view (Ctrl+Q) is the user\'s close: the tab stays closed, no crash', function()
  local v = user:view('t1')
  local r = H.perform(v.client, 'testview.close', {})
  expect(r.status, 'hub.view.close').to_be('closed')
  wait('the tab t1 suppressed, no open view', function()
    if view_of_tab(describe(), 't1') then return nil end
    for _, t in ipairs(describe().tabs or {}) do
      if t.tab == 't1' and t.suppressed then return true end
    end
    return nil
  end)
  wait_labels('Fixture home')
  check_close(v.id, 'after the pane closed')
  declare({THREE[1], THREE[2]}) -- the same revision again
  rex.sleep(2) -- the reconciler does not open it again
  expect(view_of_tab(describe(), 't1'), 'no view of t1').to_be(nil)
  expect(labels()).to_be('Fixture home')
  check_close(v.id, 'after the same revision settled')
end)

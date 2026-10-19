-- Fake Rex apps (hub.test.app): hub.app.select_session from an open form of the fixture.

local function open_form()
  local home = user:view('home')
  home:render()
  expect(home:action('open_form', 'open', {}).effects).to_be('done')
  local form = user:view('/forms/fix')
  form:render()
  return form
end

-- The target is a session that the hub made (hub.session.ensure), as a template start's new
-- session is: select_session reads it, so the hub must be attached to it.
local n = 0
local function other_window()
  n = n + 1
  local bob = agent('bob')
  local s = bob:direct('fix.ensure', {key = 'target-' .. n, name = 'Target ' .. n}).session_id
  -- Its first tab, as a template start opens it.
  local w = bob:hub('hub.spawn_window', {key = 'target-' .. n, session = s, layout = {leaf = {profile = 'shell'}}})
  return s, w.window_id
end

test('select_session needs exactly one app that shows the form\'s session', function()
  local alice = agent('alice')
  local form = open_form()
  local target, window = other_window()
  local args = {view_id = form.id, session_id = target, window_id = window}
  expect_error(function() alice:direct('fix.select', args) end, 'no_app')
  local one = app('lab-app-1', {sessions = {form.session}})
  alice:direct('fix.select', args)
  local calls = one:calls()
  expect(#calls).to_be(1)
  expect(calls[1].action).to_be('session.select')
  expect(calls[1].args.session_id).to_be(target)
  local two = app('lab-app-2', {sessions = {form.session}})
  expect_error(function() alice:direct('fix.select', args) end, 'ambiguous_app')
  two:close()
  one:fail('refused')
  expect_error(function() alice:direct('fix.select', args) end, 'refused')
  expect(#one:calls()).to_be(2)
  one:close() -- apps last for the file, like the hub
end)

test('a fake app that shows another session does not count', function()
  local alice = agent('alice')
  local form = open_form()
  local target, window = other_window()
  local elsewhere = app('lab-app-3', {sessions = {target}})
  expect_error(function() alice:direct('fix.select', {view_id = form.id, session_id = target, window_id = window}) end, 'no_app')
  expect(#elsewhere:calls()).to_be(0)
end)

test('show keeps the error that fail set', function()
  local alice = agent('alice')
  local form = open_form()
  local target, window = other_window()
  local late = app('lab-app-4', {sessions = {target}})
  late:fail('refused')
  late:show(form.session)
  expect_error(function() alice:direct('fix.select', {view_id = form.id, session_id = target, window_id = window}) end, 'refused')
  expect(#late:calls()).to_be(1)
  late:close()
end)

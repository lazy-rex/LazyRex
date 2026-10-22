-- The UI user drives real renderer panes (testview) through the hub's view path.
test('a tab renders through the production render path', function()
  local v = user:open('home')
  local tree = v:render()
  expect(node(tree, 'status').text).to_be('mode normal')
end)

test('a failed render after a good one is reported, not hidden by the kept tree', function()
  local v = user:view('home')
  v:render()
  agent('alice'):call('fix.put', {key = 'mode', value = 'fail'})
  local r = v:try_render()
  expect(r.status).to_be('failed')
  agent('alice'):call('fix.put', {key = 'mode', value = 'normal'})
  expect(node(v:render(), 'status').text).to_be('mode normal')
end)

test('a tree over the message limit fails its render generation', function()
  local v = user:view('home')
  agent('alice'):call('fix.put', {key = 'mode', value = 'big'})
  local r = v:try_render()
  expect(r.status).to_be('failed')
  expect(r.error).to_match('transient')
  agent('alice'):call('fix.put', {key = 'mode', value = 'normal'})
  v:render()
end)

test('a form effect opens a form; its submit closes the form and the result stays readable', function()
  local home = user:view('home')
  home:render()
  local op = home:action('open_form', 'open', {})
  expect(op.status).to_be('succeeded')
  expect(op.effects).to_be('done')
  local form = user:view('/forms/fix')
  form:render()
  local done = form:action('form', 'submit', {values = {value = 'hi'}})
  expect(done.status).to_be('succeeded')
  expect(store('form').value).to_equal({value = 'hi'})
  -- close_form closed the renderer: the durable record still answers.
  wait('the form view to close', function()
    for _, w in ipairs(views()) do if w.route == '/forms/fix' then return false end end
    return true
  end)
  expect(operation(done.operation_key).status).to_be('succeeded')
end)

-- events are the hub's events with this name for one view.
local function events(name, view_id)
  local out = {}
  for _, e in ipairs(H.hub('hub.test.events', {event = name}).events or {}) do
    if e.view_id == view_id then out[#out + 1] = e end
  end
  return out
end

-- open_view is the open view id, or nil (hub.test.views lists only open views).
local function open_view(id)
  for _, w in ipairs(views()) do
    if w.view_id == id then return w end
  end
  return nil
end

-- cancel ends a form as its Cancel does: the renderer exits (view/menu.go cancelForm), and the
-- hub closes the form with by = exit (ui.go). testview's exit is View:crash.
local function cancel(form)
  form:crash()
  wait('the form ' .. form.id .. ' closed', function() return open_view(form.id) == nil end)
  expect(#events('form_closed', form.id), 'form_closed of the cancelled form').to_be(1)
end

-- The home view opens form A; A's action returns {form}. The new form B takes A's place.
local function form_from_home()
  local home = user:view('home')
  home:render()
  expect(home:action('open_form', 'open', {}).effects).to_be('done')
  local a = user:view('/forms/fix')
  expect(a.context.source_view_id, 'form A names the home view').to_be(home.id)
  expect(a.context.source_route).to_be('/home')
  return home, a
end

test('a form effect from a form chains: the new form has the same window and context; the hub closes the first', function()
  local home, a = form_from_home()
  local op = a:action('chain', 'chain', {})
  expect(op.status).to_be('succeeded')
  expect(op.effects).to_be('done')
  local b = user:view('/forms/chain')
  expect(b.id ~= a.id, 'a new view').to_be(true)
  expect(b.session).to_be(a.session)
  expect(b.window).to_be(a.window)
  expect(b.context.source_view_id, 'the source view stays the home view').to_be(home.id)
  expect(b.context.source_route).to_be('/home')
  wait('form A closed', function() return open_view(a.id) == nil end)
  local closed = events('form_closed', a.id)
  expect(#closed, 'form_closed of form A').to_be(1)
  expect(closed[1].by).to_be('hub')
  expect(closed[1].why).to_be('replaced')
  b:render()
  cancel(b)
end)

test('a failed effect from a form keeps the form open and records the failure', function()
  local _, a = form_from_home()
  local op = a:action('bad_form', 'bad', {})
  expect(op.status).to_be('succeeded')
  expect(op.effects).to_be('failed')
  expect(op.effects_error).to_match('/forms/')
  local failed = {}
  for _, e in ipairs(events('view_effect_failed', a.id)) do
    if e.op_key == op.operation_key then failed[#failed + 1] = e end
  end
  expect(#failed, 'view_effect_failed of this operation').to_be(1)
  rex.sleep(1) -- a close would have happened by now
  local w = open_view(a.id)
  expect(w ~= nil, 'form A still open').to_be(true)
  expect(w.attached, 'form A still attached').to_be(true)
  expect(w.window_id).to_be(a.window)
  expect(#events('form_closed', a.id), 'form_closed of form A').to_be(0)
  cancel(a)
end)

test('an effect that fails is final and reported', function()
  local home = user:view('home')
  local op = home:action('bad_nav', 'go', {})
  expect(op.status).to_be('succeeded')
  expect(op.effects).to_be('failed')
  expect(op.effects_error).to_match('missing')
end)

-- Reviewer probe (Codex, reviewer_render.lua): a completed generation keeps its outcome.
test('a completed failed generation stays failed after later renders', function()
  local a = agent('alice')
  local v = user:view('home')
  a:call('fix.put', {key = 'mode', value = 'fail'})
  local g = H.hub('hub.test.view.render', {view_id = v.id}).generation
  local first = wait('failed generation', function()
    local x = H.hub('hub.test.view.result', {view_id = v.id, generation = g})
    if x.status ~= 'pending' then return x end
  end)
  expect(first.status).to_be('failed')
  a:call('fix.put', {key = 'mode', value = 'normal'})
  v:render()
  expect(H.hub('hub.test.view.result', {view_id = v.id, generation = g}).status).to_be('failed')
end)

test('seen reaches the plugin as view.seen through the hub', function()
  local v = user:open('home')
  v:render()
  v:seen()
  local r = wait('view.seen', function()
    local x = store('seen')
    if x.value then return x.value end
  end)
  expect(r).to_equal({route = '/home', reason = 'focus'})
end)

test('a final operation answers its key again without calling the plugin', function()
  local home = user:view('home')
  home:render()
  local first = home:action('count', 'count', {n = 1}, 'count-once')
  expect(first.status).to_be('succeeded')
  expect(first.result).to_equal({count = 1})
  -- The same key and arguments: the stored operation answers; the plugin is not called.
  expect(home:action('count', 'count', {n = 1}, 'count-once').result).to_equal({count = 1})
  expect(store('count').value).to_be(1)
  -- The same key with other arguments is refused.
  expect_error(function() home:action('count', 'count', {n = 2}, 'count-once') end, 'operation_conflict')
  expect(store('count').value).to_be(1)
end)

test('user:panel runs a command from a new pane and returns the view that it opens', function()
  local form = user:panel('fix_form')
  expect(form.route).to_be('/forms/fix')
  form:render()
  local s = session('panel-given')
  local again = user:panel('fix_form', {session = s})
  expect(again.session, 'the view is in the given session').to_be(s)
  expect(again.id ~= form.id, 'a new view').to_be(true)
end)

-- A form opened from the palette carries the source pane's directory, read once at the open
-- (decision "view render never writes; freeze open state in the form context").
test('a palette form freezes the source pane directory in its context', function()
  -- A pane that reports a directory with OSC 7 (the Profiles fixture's way: /bin/sh, no login).
  local uri = 'file:///tmp/lazyrex%20pwd'
  local created = rex.call('session.create', {label = 'source-pwd', initial_windows = {{layout = {block = {
    flavor = 'com.superlogical.terminal.shell', options = {command = {'/bin/sh', '-c', 'printf "%s" "$1"; exec sleep 100000', 'sh', '\27]7;' .. uri .. '\7'},
    login = false}}}}}})
  local s, b = created.session_id, created.initial_windows[1].block_ids[1]
  rex.call('session.attach', {session_id = s, event_scope = 'session'})
  wait('the source pane reports its directory', function()
    local r = rex.call('com.superlogical.terminal.pwd', {session_id = s, block_id = b, args = {}})
    return r and r.pwd and r.pwd ~= ''
  end)
  user:command('fix_form', {session = s, block = b})
  local form = user:view('/forms/fix', nil, s)
  expect(form.context.source_pwd, 'the raw value, not decoded').to_be(uri)
  expect(form.context.source_pwd_error).to_be(nil)
  -- The tests session's pane reports no folder (no OSC 7): the form gets the reason, and no value.
  user:command('fix_form')
  local plain = user:view("/forms/fix", nil, H.args.tests_session)
  expect(plain.context.source_pwd).to_be(nil)
  expect(plain.context.source_pwd_error:find('did not report its folder', 1, true) ~= nil, plain.context.source_pwd_error).to_be(true)
  close_session(s)
end)

-- shell_pane makes a session with one shell pane (no view in it) and returns the session, the
-- pane and its window.
local function shell_pane(label)
  local created, err = rex.call('session.create', {label = label, initial_windows = {{window_label = label,
    layout = {block = {flavor = 'com.superlogical.terminal.shell', options = {command = {'/bin/sleep', '100000'}, login = false}}}}}})
  if err then error(err, 0) end
  local w = created.initial_windows[1]
  return created.session_id, w.block_ids[1], w.window_id
end

-- open_ids is the set of open view IDs.
local function open_ids()
  local set = {}
  for _, w in ipairs(views()) do set[w.view_id] = true end
  return set
end

test('a palette run from a shell pane: the reply selects its window; the form has no source view', function()
  local s, b, win = shell_pane('palette-shell')
  local out = user:command('fix_form', {session = s, block = b})
  expect(out.status).to_be('opened')
  expect(out.select.session_id).to_be(s)
  expect(out.select.window_id).to_be(win)
  local f = user:view('/forms/fix', nil, s)
  expect(f.window).to_be(win)
  expect(f.context.source_view_id, 'no source view').to_be('')
  expect(f.context.source_block_id).to_be(b)
  cancel(f)
  close_session(s)
end)

test('a palette run from a tab: that view is the source; the tab stays open after the form ends', function()
  local home = user:view('home')
  user:command('fix_form', home)
  local f = user:view('/forms/fix', nil, home.session)
  expect(f.window).to_be(home.window)
  expect(f.context.source_view_id).to_be(home.id)
  expect(f.context.source_route).to_be('/home')
  cancel(f)
  rex.sleep(1)
  local w = open_view(home.id)
  expect(w ~= nil, 'the home view still open').to_be(true)
  expect(w.attached, 'the home view still attached').to_be(true)
end)

test('a palette run from a form: the new form takes that form\'s own source', function()
  local home, a = form_from_home()
  local before = open_ids()
  user:command('fix_form', a)
  local b = wait('the new form', function()
    for _, w in ipairs(views()) do
      if w.route == '/forms/fix' and not before[w.view_id] and w.attached then return w end
    end
    return nil
  end)
  expect(b.session_id).to_be(a.session)
  expect(b.window_id).to_be(a.window)
  local ctx = b.context
  expect(ctx.source_view_id, 'the source of form A, not form A').to_be(home.id)
  expect(ctx.source_route).to_be('/home')
  expect(ctx.source_block_id, 'the source pane of form A').to_be(a.context.source_block_id)
  wait('form A closed', function() return open_view(a.id) == nil end)
  local closed = events('form_closed', a.id)
  expect(#closed, 'form_closed of form A').to_be(1)
  expect(closed[1].by).to_be('hub')
  expect(closed[1].why).to_be('replaced')
  local bv = user:view('/forms/fix', nil, a.session) -- form A is closed: this is form B
  expect(bv.id).to_be(b.view_id)
  cancel(bv)
end)

test('a palette run is refused: unknown command, another or no client, no session, a block outside the session', function()
  local s, b = shell_pane('palette-refused')
  local before = open_ids()
  local other = rex.call('session.list_blocks', {session_id = H.args.tests_session}).blocks[1].block_id
  local command = H.args.plugin .. '.fix_form'
  expect_error(function() user:command('nope') end, 'unknown command')
  expect_error(function() H.hub('hub.palette.run', {command = command,
    ctx = {client_id = 'client:not-the-runner', session_id = s, block_id = b}}) end, 'not the calling client')
  expect_error(function() H.hub('hub.palette.run', {command = command, ctx = {session_id = s, block_id = b}}) end,
    'not the calling client')
  expect_error(function() user:command('fix_form', {}) end, 'needs a session')
  expect_error(function() user:command('fix_form', {session = s, block = other}) end, 'is not shown in session')
  expect(open_ids(), 'no view opened').to_equal(before)
  close_session(s)
end)

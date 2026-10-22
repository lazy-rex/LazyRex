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
  -- The tests session's pane sends no OSC 7: the form gets the reason, and no value.
  user:command('fix_form')
  local plain = user:view("/forms/fix", nil, H.args.tests_session)
  expect(plain.context.source_pwd).to_be(nil)
  expect(plain.context.source_pwd_error:find('reports no directory', 1, true) ~= nil, plain.context.source_pwd_error).to_be(true)
  close_session(s)
end)

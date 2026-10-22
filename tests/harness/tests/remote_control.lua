-- Remote Control, observed passively (docs/hub.md, "Remote Control"): the hub records the result of
-- each app action that it runs, and hub.app.remote_control and Hub Status show the last observed
-- result for each connected app. A fake app (hub.test.app) answers with the real app's refusal
-- text. This proves the transport and the recording, not the real app's setting.

local REFUSAL = 'Rex.app is not accepting actions. Turn on Remote Control in its Settings under Rex Server.'

local function open_form()
  local home = user:view('home')
  home:render()
  expect(home:action('open_form', 'open', {}).effects).to_be('done')
  local form = user:view('/forms/fix')
  form:render()
  return form
end

local function target_window()
  local bob = agent('bob')
  local s = bob:direct('fix.ensure', {key = 'rc-target', name = 'RC target'}).session_id
  local w = bob:hub('hub.spawn_window', {key = 'rc-target', session = s, layout = {leaf = {profile = 'shell'}}})
  return s, w.window_id
end

-- app_id is the client ID of the fake app, from client.list by its label (hub.test.app.calls gives no ID).
local function app_id(name)
  return wait('the client ID of ' .. name, function()
    for _, c in ipairs(rex.call('client.list', {}).clients or {}) do
      if c.info and c.info.label == name and c.connection_state == 'connected' then return c.client_id end
    end
  end, 10)
end

-- remote returns the hub's entry for the fake app, or nil.
local function remote(client)
  for _, a in ipairs(H.hub('hub.app.remote_control', {}).apps) do
    if a.client_id == client then return a end
  end
end

local function hub_status()
  local r = assert(rex.exec{'sh', '-c', '"$0" status -config "$1" 2>&1; echo "exit=$?"', H.args.hub_bin, H.args.lab .. '/hub.json'})
  local out = r.stdout or ''
  return out, tonumber(out:match('exit=(%d+)%s*$'))
end

test('a form app action records off on the refusal and on when the app runs it', function()
  local alice = agent('alice')
  local form = open_form()
  local target, window = target_window()
  local args = {view_id = form.id, session_id = target, window_id = window}
  local one = app('lab-rc-1', {sessions = {form.session}})
  local id = app_id('lab-rc-1')
  local a = remote(id)
  expect(a.state).to_be('unknown')
  expect(a.label).to_be('lab-rc-1')
  expect(a.at).to_be(nil)
  local out, code = hub_status()
  expect(code, out).to_be(0)
  expect(out).to_contain('Remote Control (app lab-rc-1 ' .. id .. '): unknown: no retained app action observation for this app')

  one:fail('refused', REFUSAL)
  expect_error(function() alice:direct('fix.select', args) end, 'not accepting actions')
  a = remote(id)
  expect(a.state).to_be('off')
  expect(a.action).to_be('session.select')
  out, code = hub_status()
  expect(code, out).to_be(0)
  expect(out).to_contain('Remote Control (app lab-rc-1 ' .. id .. '): last observed off at ')
  expect(out).to_contain('(session.select refused). Turn it on in the Rex app: Settings, Rex Server, Remote Control.')

  -- Another refusal says nothing about the setting: the observation stays, with its time.
  local at = a.at
  one:fail('refused', 'no grant')
  expect_error(function() alice:direct('fix.select', args) end, 'no grant')
  expect(remote(id).at).to_be(at)
  expect(remote(id).state).to_be('off')

  one:fail('')
  alice:direct('fix.select', args)
  expect(remote(id).state).to_be('on')
  out = hub_status()
  expect(out).to_contain('Remote Control (app lab-rc-1 ' .. id .. '): last observed on at ')
  one:close()
  wait('the closed app leaves the list', function() return remote(id) == nil end, 10)
end)

test('a navigation records the result of session.select on each app', function()
  local nav = app('lab-rc-2', {})
  local id = app_id('lab-rc-2')
  nav:fail('refused', REFUSAL)
  user:open('home')
  wait('the navigation reaches the app', function() return #nav:calls() > 0 end, 10)
  wait('off', function() local a = remote(id); return a and a.state == 'off' end, 10)
  nav:fail('')
  local before = #nav:calls()
  user:open('home')
  wait('the second navigation', function() return #nav:calls() > before end, 10)
  wait('on', function() local a = remote(id); return a and a.state == 'on' end, 10)
  nav:close()
end)

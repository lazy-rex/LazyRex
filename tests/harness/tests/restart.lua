-- hub:restart(): the hub stops (or crashes) and starts again on the same lab server and state.

test('a stopped hub restarts with its state; agents keep working', function()
  local alice = agent('alice')
  alice:call('fix.put', {key = 'before', value = {n = 1}})
  local old = H.hub('hub.test.status', {}).pid
  local st = hub:restart()
  expect(st.pid).not_to_be(old)
  expect(store('before').value).to_equal({n = 1})
  expect(alice:call('fix.who', {}).agent).to_be('alice')
end)

test('a crashed hub restarts; a spawn key replays and its owner stops it', function()
  local alice, bob = agent('alice'), agent('bob')
  local first = alice:hub('hub.spawn', {profile = 'shell', key = 'across'})
  hub:restart({crash = true})
  expect(alice:hub('hub.spawn', {profile = 'shell', key = 'across'}).spawn_id).to_be(first.spawn_id)
  expect_error(function() bob:hub('hub.stop', {spawn_id = first.spawn_id}) end, 'did not start')
  expect(alice:hub('hub.stop', {spawn_id = first.spawn_id}).status).to_be('closed')
end)

test('a grant made before a restart admits its request after it, as the agent', function()
  local carol, bob = agent('carol'), agent('bob')
  local layout = {leaf = {profile = 'shell'}}
  local g = carol:call('fix.grant', {key = 'run/r', entries = {{action = 'hub.spawn_window', key = 'r/1', request = {layout = layout}}}})
  hub:restart({crash = true})
  local w = carol:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'r/1', layout = layout})
  expect(w.status).to_be('open')
  for _, s in ipairs(carol:hub('hub.workers', {}).workers) do
    if s.window_id == w.window_id then
      expect(s.parent).to_be('carol')
      expect_error(function() bob:hub('hub.stop', {spawn_id = s.spawn_id}) end, 'did not start')
      expect(carol:hub('hub.stop', {spawn_id = s.spawn_id}).status).to_be('closed')
    end
  end
  -- A revoke after the restart still holds.
  expect(carol:direct('fix.grant_revoke', {grant_id = g.grant_id}).state).to_be('revoked')
  hub:restart()
  expect_error(function() carol:direct('fix.grant_spawn', {grant_id = g.grant_id, key = 'r/1', layout = layout}) end, 'revoked')
end)

-- The test renderer attaches again after a restart, as the real renderer does (view/client.go).
for _, crash in ipairs({false, true}) do
  test('a view opened before a restart answers render and action after it (crash ' .. tostring(crash) .. ')', function()
    local home = user:open('home')
    home:render()
    hub:restart({crash = crash})
    wait('the view attaches again', function()
      for _, w in ipairs(H.views()) do if w.view_id == home.id then return w.attached end end
    end, 10)
    home:render()
    local op = home:action('count', 'count', {n = 1}, 'after-restart-' .. tostring(crash))
    expect(op.status).to_be('succeeded')
  end)
end

-- The hub can ask a renderer view.release while the renderer's subscribe waits behind that request
-- in the hub's queue (Worktrees open.lua, 2026-10-19). The hold file keeps the test view between
-- its attach and its subscribe; the release must be answered then, not after a 5 s timeout.
local hold = H.args.lab .. '/testview-hold'
local function with_hold(fn)
  local f = assert(io.open(hold, 'w'))
  f:write('/forms/fix')
  f:close()
  local ok, err = pcall(fn)
  os.remove(hold)
  if not ok then error(err, 0) end
end
local function replace_form(first)
  local t0 = os.time()
  user:command('fix_form')
  expect(os.time() - t0 < 4, 'the held renderer answered view.release at once').to_be(true)
  local f = io.open(hold, 'r')
  expect(f ~= nil, 'the subscribe was still held').to_be(true)
  if f then f:close() end
  wait('the held form is replaced', function()
    for _, w in ipairs(H.views()) do if w.view_id == first.id then return nil end end
    return true
  end, 10)
  -- The new form must attach before the next command replaces it (a never-attached view cannot
  -- be replaced yet: the askrelease branch).
  user:view('/forms/fix')
end

test('a renderer answers view.release between its attach and its subscribe', function()
  with_hold(function()
    user:command('fix_form')
    replace_form(user:view('/forms/fix'))
  end)
end)

test('after a restart, a renderer answers view.release between its new attach and its subscribe', function()
  user:command('fix_form')
  local form = user:view('/forms/fix')
  with_hold(function()
    hub:restart({crash = true})
    wait('the form attaches again', function()
      for _, w in ipairs(H.views()) do if w.view_id == form.id then return w.attached end end
    end, 10)
    replace_form(form)
  end)
end)

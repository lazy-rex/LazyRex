-- Window holds (inject{match = {window = key}, hold = true}): one hub.spawn_window call of the
-- plugin waits after Rex created the window and before the hub records it. The fixture sends the
-- call from rex.after (fix.window_later), as deferred plugin work does, and records the outcome in
-- window/<key>.
-- Observed on Rex 0.0.1792158932: Rex delivers one request at a time to the hub. While a call is
-- held, the hub gets no other request until the held call's caller times out (10 s here). So a
-- test sees a hold through Rex (the labeled panes of the new window), not through the hub.

local layout = {split = {direction = 'horizontal', ratio = 0.5,
  before = {leaf = {profile = 'shell'}}, after = {leaf = {profile = 'shell'}}}}

local n = 0
local function session()
  n = n + 1
  local res, err = rex.call('session.create', {label = 'hold' .. n})
  if err then error(err.message or tostring(err)) end
  return res.session_id
end
-- spawn_panes lists the blocks of a session that carry a lazyrex-spawn label, sorted.
local function spawn_panes(s)
  local ids = {}
  for _, b in ipairs(rex.call('session.list_blocks', {session_id = s}).blocks) do
    if (b.label or ''):match('^lazyrex%-spawn:') then ids[#ids + 1] = b.block_id end
  end
  table.sort(ids)
  return ids
end
local function later(a, key, s) return a:direct('fix.window_later', {key = key, session = s, layout = layout}) end
local function outcome(key)
  return wait('the outcome of ' .. key, function() return store('window/' .. key).value end, 30)
end
local function statuses(r)
  local list = {}
  for _, l in ipairs(r.leaves) do list[#list + 1] = l.status end
  return list
end

test('a hold stops only its own key; cancel lets the held call record the window', function()
  local alice = agent('alice')
  local s = session()
  local id = inject({match = {window = 'h/held', caller = 'user'}, hold = true})
  later(alice, 'h/other', s)
  expect(outcome('h/other').result.status).to_be('open')
  expect(injection(id).consumed).to_be(0)
  later(alice, 'h/held', s)
  -- Rex created the window: four labeled panes now (two open, two held). Only Rex is asked.
  wait('the held panes', function() return #spawn_panes(s) == 4 end, 20)
  -- The next hub request waits until the held call times out for its caller.
  expect(injection(id).consumed).to_be(1)
  expect(outcome('h/held').error).to_match('timed out')
  -- Cancel: the held call goes on and records its leaves (its reply has no receiver now).
  cancel_injection(id)
  local r = wait('the recorded window', function()
    later(alice, 'h/held', s)
    local v = store('window/h/held').value
    return v and v.result and v.result.status == 'open' and v.result or nil
  end, 20)
  expect(statuses(r)).to_equal({'open', 'open'})
  -- The replays returned the recorded window and did not reach the hold again.
  expect(injection(id).consumed).to_be(1)
end)

test('a hold for another caller does not stop a user spawn', function()
  local alice = agent('alice')
  local s = session()
  local id = inject({match = {window = 'h/caller', caller = 'alice'}, hold = true})
  later(alice, 'h/caller', s)
  expect(outcome('h/caller').result.status).to_be('open')
  expect(injection(id).consumed).to_be(0)
  cancel_injection(id)
end)

test('a crash during a hold leaves the leaves pending with one candidate pane each', function()
  local alice = agent('alice')
  local s = session()
  local id = inject({match = {window = 'h/crash'}, hold = true})
  later(alice, 'h/crash', s)
  -- Only Rex is asked, and the crash comes before the held call times out for the fixture.
  wait('the held panes', function() return #spawn_panes(s) == 2 end, 20)
  local panes = spawn_panes(s)
  hub:restart({crash = true})
  -- The crash came before the held call timed out for the fixture. A timeout would have let the
  -- fixture record the error in window/h/crash; the crash ended the fixture (a hub plugin) with
  -- the hub, so nothing is recorded.
  expect(store('window/h/crash').value).to_be(nil)
  -- The restart ended the hold. The leaves are still unrecorded: the resolver never adopts an
  -- accessory pane by its label, so they stay pending with their candidates.
  local r = wait('the resolver', function()
    later(alice, 'h/crash', s)
    local v = store('window/h/crash').value
    local res = v and v.result
    if not res or not res.leaves[1].error or not res.leaves[2].error then return nil end
    return res
  end, 30)
  expect(r.status).to_be('pending')
  for _, l in ipairs(r.leaves) do
    expect(l.status).to_be('pending')
    expect(l.error).to_be('outcome unknown: 1 candidate pane(s)')
  end
  expect(spawn_panes(s)).to_equal(panes)
end)

test('a window hold is checked when it is made', function()
  expect_error(function() inject({match = {writes = 'x'}, hold = true}) end, 'hold needs match')
  expect_error(function() inject({match = {window = 'k'}}) end, 'needs hold = true')
  expect_error(function() inject({match = {window = 'k'}, hold = true, fail = 'x'}) end, 'has no fail')
  expect_error(function() inject({match = {window = 'k'}, hold = true, fail_code = 'x'}) end, 'has no fail_code')
  expect_error(function() inject({match = {window = 'k'}, hold = true, times = 2}) end, 'has no times')
  expect_error(function() inject({match = {window = 'k', op_key = 'x'}, hold = true}) end, 'only window and caller')
end)

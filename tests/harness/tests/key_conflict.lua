-- Key conflicts on the real path (docs/hub.md, lead-b3 B3'): each keyed hub action, called twice with
-- the same key and another request through the real hub and Rex, answers the second call with the
-- code operation_conflict and the message "operation_conflict: <text>". The first call is real too.
-- hub_test (keyconflict_test.go) maps every conflict site of the hub to this file or to its test.

local alice = agent('alice')

-- hubcall is a hub action as the fixture plugin; the answer is {result} or {error = {code, message}}.
local function hubcall(action, args, on_behalf)
  return alice:call('fix.hub', {action = action, args = args, on_behalf = on_behalf})
end

local function ok(name, r)
  if r.error then fail(name .. ': ' .. rex.json.encode(r.error)) end
  return r.result
end

-- conflict checks the code and that the message holds the exact full text.
local function conflict(name, e, text)
  local want = 'operation_conflict: ' .. text
  expect(e and e.code, name .. ' code').to_be('operation_conflict')
  if not tostring(e.message):find(want, 1, true) then
    fail(name .. ': the message ' .. rex.json.encode(e.message) .. ' does not hold ' .. rex.json.encode(want))
  end
  print('OBSERVED ' .. name .. ': ' .. tostring(e.message))
end

test('setup: a worker profile', function()
  write_profiles({profiles = {{id = 'probe', name = 'Probe', kind = 'accessory', command = {'/bin/sh', '-c', 'exec sleep 600'}}}})
end)

test('hub.spawn', function()
  local s = ok('first', hubcall('hub.spawn', {['for'] = 'user', profile = 'probe', key = 'kc-spawn'}))
  local r = hubcall('hub.spawn', {['for'] = 'user', profile = 'probe', key = 'kc-spawn', title = 'Other'})
  conflict('hub.spawn', r.error, 'key "kc-spawn" was used for a different request')
  H.hub('hub.stop', {spawn_id = s.spawn_id})
end)

test('hub.spawn_window', function()
  local sess = assert(rex.call('session.create', {label = 'kc-window'})).session_id
  local layout = {leaf = {profile = 'probe'}}
  local w = ok('first', hubcall('hub.spawn_window', {['for'] = 'user', key = 'kc-window', session = sess, layout = layout}))
  local r = hubcall('hub.spawn_window', {['for'] = 'user', key = 'kc-window', session = sess, title = 'Other', layout = layout})
  conflict('hub.spawn_window', r.error, 'key "kc-window" was used for a different request')
  for _, l in ipairs(w.leaves or {}) do
    if l.spawn_id then H.hub('hub.stop', {spawn_id = l.spawn_id}) end
  end
end)

test('hub.session.ensure', function()
  ok('first', hubcall('hub.session.ensure', {key = 'kc-ensure', name = 'KC Work'}))
  local r = hubcall('hub.session.ensure', {key = 'kc-ensure', name = 'KC Other'})
  conflict('hub.session.ensure', r.error, 'key "kc-ensure" was used with another name')
end)

test('hub.grant.create', function()
  local function entries(title)
    return {{action = 'hub.spawn_window', key = 'kc/w1', request = {title = title, layout = {leaf = {profile = 'probe'}}}}}
  end
  ok('first', hubcall('hub.grant.create', {key = 'kc-grant', entries = entries('Dev')}, true))
  local r = hubcall('hub.grant.create', {key = 'kc-grant', entries = entries('Other')}, true)
  conflict('hub.grant.create', r.error, 'the grant key "kc-grant" was used for another agent or other requests')
end)

test('hub.sql.commit', function()
  local tx = ok('begin', hubcall('hub.sql.begin', {})).tx
  ok('first', hubcall('hub.sql.commit', {tx = tx, key = 'kc-sql', fingerprint = 'f1'}))
  tx = ok('begin again', hubcall('hub.sql.begin', {})).tx
  local r = hubcall('hub.sql.commit', {tx = tx, key = 'kc-sql', fingerprint = 'f2'})
  hubcall('hub.sql.rollback', {tx = tx}) -- a refused commit can leave its transaction open
  conflict('hub.sql.commit', r.error, 'the key kc-sql was used with another fingerprint')
end)

test('hub.admin.resume', function()
  H.hub('hub.admin.resume', {key = 'kc-resume', kind = 'codex'})
  local e = H.expect_error(function() H.hub('hub.admin.resume', {key = 'kc-resume', release = 'none'}) end)
  conflict('hub.admin.resume', e, 'key "kc-resume" was used for a different request')
end)

test('hub.view.action', function()
  local home = user:view('home')
  home:render()
  home:action('count', 'count', {n = 1}, 'kc-view')
  local e = H.expect_error(function() home:action('count', 'count', {n = 2}, 'kc-view') end)
  conflict('hub.view.action', e, 'key "kc-view" has different arguments')
end)

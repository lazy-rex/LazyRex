-- Test-mode faults (docs/testing.md, "Faults"): spawn-create and action faults fire once, match
-- only their target, and leave durable state that a same-key retry recovers. The fixture's
-- fix.hub calls a hub action as the plugin and returns the hub's answer as data.

-- kv returns the decoded value of the fixture's kv row key (nil: no row).
local function kv(key)
  local row = sql('SELECT value FROM kv WHERE key = ?', {key})[1]
  return row and rex.json.decode(row.value)
end

local alice = agent('alice')
local lockdir = H.args.lab .. '/tmp' -- not the cwd of any worker pane (workers start in HOME)

local function hubcall(action, args)
  return alice:call('fix.hub', {action = action, args = args})
end

local function spawn(key)
  return hubcall('hub.spawn', {profile = 'probe', key = key, ['for'] = 'user'})
end

-- panes counts the panes that a spawn created (label lazyrex-spawn:<id>).
local function panes(session_id, spawn_id)
  local r = rex.call('session.list_blocks', {session_id = session_id})
  local n = 0
  for _, b in ipairs(r and r.blocks or {}) do
    if b.label == 'lazyrex-spawn:' .. spawn_id then n = n + 1 end
  end
  return n
end

-- rows counts the spawn rows (hub.workers lists every row; it has no key field).
local function rows()
  return #(hubcall('hub.workers', {}).result.workers or {})
end

test('setup: a worker profile', function()
  write_profiles({profiles = {{id = 'probe', name = 'Probe', kind = 'accessory', command = {'/bin/sh', '-c', 'exec sleep 600'}}}})
end)

test('arming refuses invalid faults and an unknown fault id', function()
  local cases = {
    {{point = 'nope', mode = 'drop'}, 'point must be'},
    {{point = 'spawn-create', mode = 'fail'}, 'point must be'},
    {{point = 'spawn-create', mode = 'drop', when = 'before'}, 'does not apply'},
    {{point = 'action', action = 'hub.spawn', when = 'before', mode = 'fail', op = 'x'}, 'does not apply'},
    {{point = 'action', action = 'hub.spawn', when = 'during', mode = 'fail'}, 'when must be'},
    {{point = 'action', action = 'hub.nope', when = 'before', mode = 'fail'}, 'not a hub action'},
    {{point = 'action', action = 'hub.test.write', when = 'before', mode = 'fail'}, 'not a hub action'},
    {{point = 'action', action = 'hub.spawn', when = 'before', mode = 'fail', plugin = 'nope'}, 'unknown plugin'},
    {{point = 'spawn-create', mode = 'drop', op = 7}, 'must be a string'},
  }
  for _, c in ipairs(cases) do expect_error(function() fault(c[1]) end, c[2]) end
  expect_error(function() fault_fired(999) end, 'no fault')
  local id = fault({point = 'spawn-create', mode = 'drop', op = 'f-fraction'})
  expect_error(function() fault_fired(id + 0.5) end, 'no fault')
  expect(spawn('f-fraction').result.status).to_be('pending')
end)

test('spawn-create drop: no pane; the spawn stays pending', function()
  local id = fault({point = 'spawn-create', mode = 'drop', op = 'f-drop'})
  local before = rows()
  local r = spawn('f-drop').result
  expect(r.status).to_be('pending')
  expect(fault_fired(id)).to_be(true)
  expect(panes(r.session_id, r.spawn_id)).to_be(0)
  expect(rows()).to_be(before + 1)
end)

test('spawn-create refuse: no pane; the spawn fails', function()
  fault({point = 'spawn-create', mode = 'refuse', op = 'f-refuse'})
  local r = spawn('f-refuse').result
  expect(r.status).to_be('failed')
  expect(r.error).to_match('injected fault')
  expect(panes(r.session_id, r.spawn_id)).to_be(0)
end)

test('spawn-create lose-answer: one pane, pending; a same-key retry keeps one row and one pane', function()
  local id = fault({point = 'spawn-create', mode = 'lose-answer', op = 'f-lose'})
  local before = rows()
  local r = spawn('f-lose').result
  expect(r.status, 'the outcome is unknown').to_be('pending')
  expect(fault_fired(id)).to_be(true)
  wait('the pane exists', function() return panes(r.session_id, r.spawn_id) == 1 end)
  local again = spawn('f-lose').result
  expect(again.spawn_id).to_be(r.spawn_id)
  expect(again.status, 'an accessory pane is not adopted by its label').to_be('pending')
  expect(rows()).to_be(before + 1)
  expect(panes(r.session_id, r.spawn_id)).to_be(1)
end)

test('the op filter: another key is not affected and the fault stays armed', function()
  local id = fault({point = 'spawn-create', mode = 'drop', op = 'f-other'})
  local r = spawn('f-normal').result
  expect(r.status).to_be('open')
  expect(fault_fired(id), 'still armed').to_be(false)
  expect(spawn('f-other').result.status).to_be('pending')
  expect(fault_fired(id)).to_be(true)
end)

test('hub.spawn after/lose-reply: the spawn commits, the reply is disconnected, a retry recovers it', function()
  local id = fault({point = 'action', action = 'hub.spawn', when = 'after', mode = 'lose-reply', key = 'f-after'})
  local before = rows()
  local lost = spawn('f-after')
  expect(lost.error.code).to_be('disconnected')
  expect(fault_fired(id)).to_be(true)
  expect(rows(), 'the spawn is durable').to_be(before + 1)
  local r = spawn('f-after').result
  expect(r.status).to_be('open')
  expect(rows(), 'no second spawn').to_be(before + 1)
  expect(panes(r.session_id, r.spawn_id)).to_be(1)
end)

test('hub.cwd.unlock before/fail: the lock stays; one shot, so a retry unlocks', function()
  expect(hubcall('hub.cwd.lock', {path = lockdir, key = 'k1'}).result.locked).to_be(true)
  local id = fault({point = 'action', action = 'hub.cwd.unlock', when = 'before', mode = 'fail', key = 'k1'})
  local e = hubcall('hub.cwd.unlock', {path = lockdir, key = 'k1'}).error
  expect(e.code).to_be('failed')
  expect(fault_fired(id)).to_be(true)
  expect(hubcall('hub.cwd.lock', {path = lockdir, key = 'k2'}).error.message, 'still locked by k1').to_match('conflict')
  expect(hubcall('hub.cwd.unlock', {path = lockdir, key = 'k1'}).result.locked).to_be(false)
  expect(hubcall('hub.cwd.lock', {path = lockdir, key = 'k2'}).result.locked).to_be(true)
  hubcall('hub.cwd.unlock', {path = lockdir, key = 'k2'})
end)

test('a wrong key is not affected', function()
  local id = fault({point = 'action', action = 'hub.cwd.lock', when = 'before', mode = 'fail', key = 'right'})
  expect(hubcall('hub.cwd.lock', {path = lockdir, key = 'wrong'}).result.locked).to_be(true)
  hubcall('hub.cwd.unlock', {path = lockdir, key = 'wrong'})
  expect(fault_fired(id)).to_be(false)
  expect(hubcall('hub.cwd.lock', {path = lockdir, key = 'right'}).error.code).to_be('failed')
  expect(fault_fired(id)).to_be(true)
end)

test('a wrong plugin is not affected', function()
  -- The home plugin calls hub.workers when it renders; the fixture's call is not affected.
  local id = fault({point = 'action', action = 'hub.workers', when = 'before', mode = 'fail', plugin = 'lazyrex'})
  expect(hubcall('hub.workers', {}).result).to_be_truthy()
  expect(fault_fired(id)).to_be(false)
  user:panel('agents', {plugin = 'lazyrex'}):render()
  wait('the home plugin call fires the fault', function() return fault_fired(id) end)
end)

test('after with a genuine handler error: that error is the reply and the fault stays armed', function()
  local id = fault({point = 'action', action = 'hub.cwd.unlock', when = 'after', mode = 'fail', key = 'g'})
  local e = hubcall('hub.cwd.unlock', {path = 'relative', key = 'g'}).error
  expect(e.code).to_be('refused')
  expect(e.message).to_match('absolute')
  expect(fault_fired(id)).to_be(false)
  expect(hubcall('hub.cwd.unlock', {path = lockdir, key = 'g'}).error.code).to_be('failed')
  expect(fault_fired(id)).to_be(true)
end)

test('an agent-face call is not affected by a plugin action fault', function()
  local id = fault({point = 'action', action = 'hub.spawn', when = 'before', mode = 'fail', key = 'f-face'})
  local s = alice:call('hub.spawn', {profile = 'probe', key = 'f-face'})
  expect(s.spawn_id).to_be_truthy()
  expect(fault_fired(id)).to_be(false)
  expect(spawn('f-face').error.code).to_be('failed')
  expect(fault_fired(id)).to_be(true)
  expect(spawn('f-face').result.spawn_id, 'one shot').to_be_truthy()
end)

test('fail_code: an action fault fails with the chosen code; the default stays failed', function()
  expect_error(function() fault({point = 'action', action = 'hub.cwd.lock', when = 'before', mode = 'lose-reply', fail_code = 'unavailable'}) end, 'needs mode fail')
  expect_error(function() fault({point = 'action', action = 'hub.cwd.lock', when = 'before', mode = 'fail', fail_code = ''}) end, 'nonempty')
  expect_error(function() fault({point = 'spawn-create', mode = 'refuse', fail_code = 'unavailable'}) end, 'does not apply')
  local id = fault({point = 'action', action = 'hub.cwd.lock', when = 'before', mode = 'fail', key = 'fc', fail_code = 'unavailable'})
  local e = hubcall('hub.cwd.lock', {path = lockdir, key = 'fc'}).error
  expect(e.code, 'a transient code').to_be('unavailable')
  expect(fault_fired(id)).to_be(true)
  expect(hubcall('hub.cwd.lock', {path = lockdir, key = 'fc'}).result.locked, 'the retry succeeds').to_be(true)
  hubcall('hub.cwd.unlock', {path = lockdir, key = 'fc'})
end)

test('fail_code on an injection: a commit fails with the chosen code; the default stays injected_failure', function()
  local write = {sql = 'INSERT INTO kv VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value', params = {'fc/x', '1'}}
  local match = {sql_commit_writes = 'kv', sql_param = 'fc/x'}
  expect_error(function() inject({match = match, fail_code = 'unavailable'}) end, 'needs fail')
  expect_error(function() inject({match = match, fail = 'x', fail_code = ''}) end, 'nonempty')
  local e
  with_injection({match = match, fail = 'definite'}, 1, function() e = hubcall('hub.sql.exec', write).error end)
  expect(e.code, 'the default code').to_be('injected_failure')
  with_injection({match = match, fail = 'transient', fail_code = 'unavailable'}, 1, function() e = hubcall('hub.sql.exec', write).error end)
  expect(e.code).to_be('unavailable')
  expect(e.message).to_match('transient')
  expect(kv('fc/x'), 'a failed commit commits nothing').to_be(nil)
  expect(hubcall('hub.sql.exec', write).result, 'the retry commits').to_be_truthy()
  expect(kv('fc/x')).to_be(1)
  local iid = inject({match = {sql_commit_writes = 'kv', sql_param = 'fc/z'}, fail = 'fraction'})
  expect_error(function() injection(iid + 0.5) end, 'no injection')
  expect(hubcall('hub.sql.exec', {sql = 'INSERT INTO kv VALUES (?, ?)', params = {'fc/z', '1'}}).error.code).to_be('injected_failure')
end)

test('nth: the fault fires on the n-th matching call', function()
  expect_error(function() fault({point = 'action', action = 'hub.cwd.lock', when = 'before', mode = 'fail', nth = 0}) end, 'positive integer')
  expect_error(function() fault({point = 'action', action = 'hub.cwd.lock', when = 'before', mode = 'fail', nth = 1.5}) end, 'positive integer')
  expect_error(function() fault({point = 'spawn-create', mode = 'drop', nth = 2}) end, 'does not apply')
  local id = fault({point = 'action', action = 'hub.cwd.lock', when = 'before', mode = 'fail', key = 'nth', nth = 2})
  expect(hubcall('hub.cwd.lock', {path = lockdir, key = 'nth'}).result.locked, 'the first call is normal').to_be(true)
  expect(fault_fired(id)).to_be(false)
  expect(hubcall('hub.cwd.lock', {path = lockdir, key = 'nth'}).error.code).to_be('failed')
  expect(fault_fired(id)).to_be(true)
  hubcall('hub.cwd.unlock', {path = lockdir, key = 'nth'})
end)

test('gate: matching calls get unavailable until the release; a retrying caller then passes', function()
  expect_error(function() fault({point = 'action', action = 'hub.sql.exec', when = 'after', mode = 'gate'}) end, 'needs when before')
  local plain = fault({point = 'action', action = 'hub.cwd.lock', when = 'before', mode = 'fail', key = 'gate-plain'})
  expect_error(function() fault_release(plain) end, 'not a closed gate')
  hubcall('hub.cwd.lock', {path = lockdir, key = 'gate-plain'})
  user:command('fix_form')
  local form = user:view('/forms/fix')
  form:render()
  local id = fault({point = 'action', action = 'hub.sql.exec', when = 'before', mode = 'gate'})
  -- A view operation: the hub retries an unavailable outcome every 500 ms.
  local _, key = form:admit('action', {node_id = 'form', action = 'submit', input = {values = {value = 'gated'}}, revision = form.revision})
  wait('the gate refuses a retry too', function() return H.hub('hub.test.fault_state', {id = id}).gated >= 2 end)
  expect(operation(key).status, 'the operation waits at the gate').to_be('pending')
  expect(kv('form'), 'nothing committed').to_be(nil)
  expect(hubcall('hub.sql.exec', {sql = 'INSERT INTO kv VALUES (?, ?)', params = {'gate/x', '1'}}).error.code, 'a direct call is refused too').to_be('unavailable')
  local r = fault_release(id)
  expect(r.released).to_be(true)
  expect_error(function() fault_release(id) end, 'not a closed gate')
  expect(form:settle(key).status, 'the retry after the release succeeds').to_be('succeeded')
  expect(kv('form')).to_equal({value = 'gated'})
end)

test('gate: a gate that has not fired cannot be released', function()
  local id = fault({point = 'action', action = 'hub.cwd.lock', when = 'before', mode = 'gate', key = 'gate-early'})
  expect_error(function() fault_release(id) end, 'has not fired')
  expect(hubcall('hub.cwd.lock', {path = lockdir, key = 'gate-early'}).error.code, 'the gate still fires').to_be('unavailable')
  expect(fault_release(id).released).to_be(true)
end)

test('gate: a call that a closed gate refuses counts for no other fault', function()
  local function state(id) return H.hub('hub.test.fault_state', {id = id}) end
  local gate = fault({point = 'action', action = 'hub.cwd.lock', when = 'before', mode = 'gate', key = 'gate-count'})
  local later = fault({point = 'action', action = 'hub.cwd.lock', when = 'before', mode = 'fail', key = 'gate-count', nth = 2})
  local call = function() return hubcall('hub.cwd.lock', {path = lockdir, key = 'gate-count'}) end
  expect(call().error.code, 'the gate fires').to_be('unavailable')
  expect(state(later).seen, 'the call that fired the gate counts').to_be(1)
  expect(call().error.code, 'the gate is closed').to_be('unavailable')
  expect(state(later).seen, 'a refused call does not count').to_be(1)
  expect(state(gate).gated).to_be(2)
  fault_release(gate)
  expect(call().error.code, 'the second counted call fires the fail fault').to_be('failed')
  expect(fault_fired(later)).to_be(true)
end)

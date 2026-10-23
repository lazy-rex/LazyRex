-- A notice reaches an agent whose program status the hub read too early or not at all (defect:
-- notices stalled for idle agents on the live hub, 2026-10-20). Each agent here is a pane in its
-- own session that never joins: the hub must attach that session before it can read the pane's
-- status (rex-protocol.md, "Program status needs an attached session"). Unknown is never ready.

local function written(id)
  local n = 0
  for _, e in ipairs(H.hub('hub.test.events', {event = 'notice_written'}).events or {}) do
    if e.agent == id then n = n + 1 end
  end
  return n
end

local function reads(id)
  local out = {}
  for _, e in ipairs(H.hub('hub.test.events', {event = 'agent_status_read'}).events or {}) do
    if e.agent == id then out[#out + 1] = e end
  end
  return out
end

local function activity(id)
  for _, a in ipairs(H.hub('hub.agents', {all = true}).agents or {}) do
    if a.id == id then return a.activity end
  end
end

-- notify sets a pending notice of the plugin under test for the agent id.
local function notify(a, id)
  local r = a:call('fix.hub', {action = 'hub.store.batch', args = {ops = {{notify = {agent = id, pending = true}}}}})
  expect(r.error, 'the notify batch').to_be(nil)
end

-- far makes a pane in a new session, reports it as the identity claude:<conv>, and returns it.
local function far(name, conv, status)
  local s = H.session(name)
  local a = agent(name, {report = false, session = s})
  if status then a:program_status(status) end
  a:report({kind = 'claude', conversation_id = conv})
  return a, s
end

test('an agent idle across a hub restart gets a notice with no status change', function()
  local a = far('idle-far', 'idle-1', 'idle')
  wait('the hub knows the agent is idle', function() return activity('claude:idle-1') == 'idle' end)
  hub:restart()
  wait('the hub reads the status again after the restart', function() return activity('claude:idle-1') == 'idle' end)
  notify(a, 'claude:idle-1')
  wait('the notice is written', function() return written('claude:idle-1') == 1 end)
end)

test('a failed status read is retried, then the notice is written', function()
  local s = H.session('retry-far')
  fault({point = 'status-read', mode = 'fail', key = s}) -- the read after the report
  fault({point = 'status-read', mode = 'fail', key = s}) -- noticeLoop's first read
  local a = agent('retry-far', {report = false, session = s})
  a:program_status('idle')
  a:report({kind = 'claude', conversation_id = 'retry-1'})
  expect(activity('claude:retry-1'), 'the state is unknown after a failed read').to_be('')
  notify(a, 'claude:retry-1')
  wait('the notice is written after a retry', function() return written('claude:retry-1') == 1 end, 20)
  local r = reads('claude:retry-1')
  expect(#r, 'one failed read, then one applied read').to_be(2)
  expect(r[1].applied).to_be(false)
  expect(r[1].error).to_be('injected fault: the status read failed')
  expect(r[2].applied).to_be(true)
  expect(r[2].state).to_be('idle')
end)

test('a blocked agent gets no notice until it is idle', function()
  local a = far('blocked-far', 'blocked-1')
  a:program_status('blocked')
  wait('the hub knows the agent is blocked', function() return activity('claude:blocked-1') == 'blocked' end)
  notify(a, 'claude:blocked-1')
  rex.sleep(1.5)
  expect(written('claude:blocked-1'), 'no notice for a blocked prompt').to_be(0)
  a:program_status('idle')
  wait('the notice is written when the agent is idle', function() return written('claude:blocked-1') == 1 end)
end)

test('a status event during the read wins over the older result', function()
  local s = H.session('race-far')
  fault({point = 'status-read', mode = 'fail', key = s}) -- the read after the report: unknown
  local hold = fault({point = 'status-read', mode = 'hold', key = s})
  local a = agent('race-far', {report = false, session = s})
  a:program_status('idle')
  a:report({kind = 'claude', conversation_id = 'race-1'})
  notify(a, 'claude:race-1')
  wait('noticeLoop read idle and holds', function() return H.fault_fired(hold) end)
  a:program_status('blocked')
  wait('the hub knows the agent is blocked', function() return activity('claude:race-1') == 'blocked' end)
  H.fault_release(hold)
  wait('the older read is not applied', function() return #reads('claude:race-1') == 1 end)
  expect(reads('claude:race-1')[1].applied).to_be(false)
  rex.sleep(1.5)
  expect(written('claude:race-1'), 'the blocked state stands').to_be(0)
  expect(activity('claude:race-1')).to_be('blocked')
  a:program_status('idle')
  wait('the notice is written when the agent is idle', function() return written('claude:race-1') == 1 end)
end)

test('a pane with no status record is retried, not treated as ready', function()
  local a = far('silent-far', 'silent-1')
  notify(a, 'claude:silent-1')
  wait('noticeLoop read the status', function() return #reads('claude:silent-1') >= 1 end)
  expect(reads('claude:silent-1')[1].error).to_be('no program status record')
  rex.sleep(1.5)
  expect(written('claude:silent-1'), 'unknown is not ready').to_be(0)
  a:program_status('idle')
  wait('the notice is written when the agent reports idle', function() return written('claude:silent-1') == 1 end)
end)

test('a plugin stop during the read writes no notice; the notice waits for the next run', function()
  local s = H.session('stop-far')
  fault({point = 'status-read', mode = 'fail', key = s}) -- the read after the report: unknown
  local hold = fault({point = 'status-read', mode = 'hold', key = s})
  local a = agent('stop-far', {report = false, session = s})
  a:program_status('idle')
  a:report({kind = 'claude', conversation_id = 'stop-1'})
  notify(a, 'claude:stop-1')
  wait('noticeLoop read idle and holds', function() return H.fault_fired(hold) end)
  -- The stop clears the plugin's run (its notice) while the read holds.
  H.hub('hub.admin.stop', {plugin = 'harnessfix'})
  H.fault_release(hold)
  wait('the read is applied', function() return #reads('claude:stop-1') == 1 end)
  rex.sleep(1.5)
  expect(written('claude:stop-1'), 'no notice without a run').to_be(0)
  H.hub('hub.admin.start', {plugin = 'harnessfix'})
  wait('the plugin', function() return H.hub('hub.test.status', {}).plugins.harnessfix.registered end)
  wait('the notice is written for the new run', function() return written('claude:stop-1') == 1 end)
end)

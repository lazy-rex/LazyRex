-- One nudge until a read (the user's rule, 2026-10-22). A send with once:true makes no new nudge
-- while the agent has an unread nudge; {read = true} ends that. A nudge that the hub wrote is not
-- written again after a hub restart. A plain pending:true keeps one nudge for each send.

local function written(id)
  local n = 0
  for _, e in ipairs(H.hub('hub.test.events', {event = 'notice_written'}).events or {}) do
    if e.agent == id then n = n + 1 end
  end
  return n
end

local function activity(id)
  for _, a in ipairs(H.hub('hub.agents', {all = true}).agents or {}) do
    if a.id == id then return a.activity end
  end
end

local function op(a, notify)
  local r = a:call('fix.hub', {action = 'hub.store.batch', args = {ops = {{notify = notify}}}})
  expect(r.error, 'the notify batch').to_be(nil)
end

local function far(name, conv)
  local s = H.session(name)
  local a = agent(name, {join = false, session = s})
  a:program_status('idle')
  a:report({kind = 'claude', conversation_id = conv})
  wait('the hub knows the agent is idle', function() return activity('claude:' .. conv) == 'idle' end)
  return a
end

test('three once sends give one nudge; a read, then a send, gives one more', function()
  local id = 'claude:once-1'
  local a = far('once-far', 'once-1')
  op(a, {agent = id, pending = true, once = true})
  wait('the first nudge', function() return written(id) == 1 end)
  op(a, {agent = id, pending = true, once = true})
  op(a, {agent = id, pending = true, once = true})
  rex.sleep(1.5)
  expect(written(id), 'no nudge while the first is unread').to_be(1)
  op(a, {agent = id, read = true})
  rex.sleep(1.5)
  expect(written(id), 'a read writes no nudge').to_be(1)
  op(a, {agent = id, pending = true, once = true})
  wait('a send after the read gives a nudge', function() return written(id) == 2 end)
  -- A plain pending:true keeps today's rule: one nudge for each send.
  op(a, {agent = id, pending = true})
  wait('a plain send gives a nudge', function() return written(id) == 3 end)
end)

test('a hub restart does not write a nudge again', function()
  local id = 'claude:once-2'
  local a = far('restart-far', 'once-2')
  op(a, {agent = id, pending = true, once = true})
  wait('the nudge', function() return written(id) == 1 end)
  hub:restart()
  wait('the hub reads the status again after the restart', function() return activity(id) == 'idle' end)
  rex.sleep(2)
  expect(written(id), 'no nudge again after the restart').to_be(1)
  op(a, {agent = id, read = true})
  op(a, {agent = id, pending = true, once = true})
  wait('a send after a read gives a nudge after the restart', function() return written(id) == 2 end)
end)

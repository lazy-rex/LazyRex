-- Agent identity reports (hub/cmd/hub/identity.go). Each report runs in a new process in the
-- agent's lab pane, so the pid proof and the process start order are real
-- (observed on Rex 0.0.1792158932-g88fd41).

local function ident(id)
  local list = H.hub('hub.identity.list', {}).identities or {}
  for _, x in ipairs(list) do
    if x.identity == id then return x end
  end
  return nil
end

local function block_of(id) return ident(id).block_id end

test('startup, a repeat, and clear: current, unchanged, then a successor', function()
  local a = agent('id-start', {report = false})
  local r = a:report({kind = 'claude', conversation_id = 'conv-s1'})
  expect(r.state).to_be('current')
  expect(a:report({kind = 'claude', conversation_id = 'conv-s1'}).state).to_be('unchanged')
  r = a:report({kind = 'claude', conversation_id = 'conv-s2', source = 'clear'})
  expect(r.state).to_be('current')
  expect(r.predecessor, '/clear names the old identity').to_be('claude:conv-s1')
  expect(ident('claude:conv-s1').state).to_be('superseded')
end)

test('a delayed report from an older process is stale and records nothing', function()
  local a = agent('id-stale', {report = false})
  a:report({kind = 'claude', conversation_id = 'conv-t1'})
  -- The older process has started when report_async returns; it waits, and a newer process
  -- reports an unseen conversation first.
  a:report_async('late', {kind = 'claude', conversation_id = 'conv-late', source = 'prompt'}, 2000)
  local r = a:report({kind = 'claude', conversation_id = 'conv-t2', source = 'prompt'})
  expect(r.state, 'a prompt with an unseen ID repairs a lost /clear').to_be('current')
  local late = a:report_result('late')
  expect(late.error and late.error.message or '', 'the older process is stale').to_match('stale')
  expect(ident('claude:conv-late'), 'a stale report records no identity').to_be(nil)
end)

test('a newer hook report revives a superseded identity', function()
  local a = agent('id-revive', {report = false})
  a:report({kind = 'claude', conversation_id = 'conv-m1'})
  expect(a:report({kind = 'claude', conversation_id = 'conv-m2', source = 'clear'}).predecessor).to_be('claude:conv-m1')
  a:report({kind = 'claude', conversation_id = 'conv-m3', source = 'clear'})
  expect(ident('claude:conv-m2').state).to_be('superseded')
  local r = a:report({kind = 'claude', conversation_id = 'conv-m2', source = 'resume'})
  expect(r.state, 'a lost resume is repaired').to_be('current')
  expect(r.predecessor, 'a revived identity keeps its predecessor').to_be('claude:conv-m1')
  expect(ident('claude:conv-m3').state).to_be('superseded')
end)

test('another kind in the same pane is a transition; a wrong block hint is ignored', function()
  local a = agent('id-kind', {report = false})
  local other = agent('id-other', {report = false})
  other:report({kind = 'claude', conversation_id = 'conv-o1'})
  a:report({kind = 'claude', conversation_id = 'conv-k1'})
  local mine = block_of('claude:conv-k1')
  local r = a:report({kind = 'codex', conversation_id = 'thread-k1', block = block_of('claude:conv-o1')})
  expect(r.state).to_be('current')
  expect(ident('codex:thread-k1').block_id, 'the ancestry decides the pane').to_be(mine)
  expect(ident('claude:conv-k1').state).to_be('superseded')
  expect(ident('claude:conv-o1').state, 'the other pane is not touched').to_be('current')
end)

test('source mcp is refused: identities come from the hooks only', function()
  local a = agent('id-mcp', {report = false})
  expect_error(function() a:report({kind = 'codex', conversation_id = 'thread-x1', source = 'mcp'}) end, 'invalid: source')
  expect(ident('codex:thread-x1')).to_be(nil)
end)

test('the identity takes the tab label as its name', function()
  local a = agent('carol', {title = 'boot', report = false})
  local r = a:report({kind = 'codex', conversation_id = 'thread-boot'})
  expect(r.state).to_be('current')
  expect(r.name, 'the name is the tab label').to_be('boot')
end)

test('a caller outside any pane is not_in_pane', function()
  expect_error(function() H.hub('hub.identity.report', {kind = 'claude', conversation_id = 'conv-out', source = 'startup'}) end, 'not_in_pane')
  expect(ident('claude:conv-out')).to_be(nil)
end)

test('the agent process exits in a kept-open pane: child_exited detaches its identity', function()
  local a = agent('id-exit', {report = false, keep_open = true})
  expect(a:report({kind = 'claude', conversation_id = 'conv-x1'}).state).to_be('current')
  local block = block_of('claude:conv-x1')
  a:exit()
  wait('the identity is detached', function() return ident('claude:conv-x1').state == 'detached' end)
  local found = false
  for _, b in ipairs(rex.call('session.list_blocks', {session_id = a.session}).blocks or {}) do
    found = found or b.block_id == block
  end
  expect(found, 'the pane stays: the detach comes from the process exit, not a pane close').to_be(true)
  rex.call('block.close', {session_id = a.session, block_id = block})
end)

test('a tab rename renames the identity; a pane close detaches it and keeps its record', function()
  local a = agent('id-close', {report = false})
  a:report({kind = 'codex', conversation_id = 'thread-c1'})
  a:set_label('renamed tab')
  wait('the name follows the tab', function() return ident('codex:thread-c1').name == 'renamed tab' end)
  local _, err = rex.call('block.close', {session_id = a.session, block_id = block_of('codex:thread-c1')})
  expect(err).to_be(nil)
  wait('the identity is detached', function() return ident('codex:thread-c1').state == 'detached' end)
  expect(ident('codex:thread-c1').name, 'the record stays').to_be('renamed tab')
end)

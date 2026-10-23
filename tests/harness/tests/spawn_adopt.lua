-- Spawn adoption by identity report (docs/hub.md, "Workers"): an agent spawn whose create answer
-- was lost stays pending. Its program reports an identity from the one pane with the spawn's label,
-- and the hub opens the spawn there, also after a hub crash. The fake agent CLI (`lazyrex-hub
-- testagent cli`, hub/cmd/hub/testcli.go) reports after its first input line, so the test decides
-- when the report comes. It reports to the one connected hub: after a crash Rex keeps the lost hub
-- client for a while, and a report by the name lazyrex-hub alone would be ambiguous.

local alice = agent('alice')
local hub_bin = H.args.hub_bin
local lab = H.args.lab

write_profiles({profiles = {{id = 'reporter', name = 'reporter', kind = 'agent', cwd = lab,
  command = {'/bin/sh', '-c', 'exec "$0" testagent cli codex "$1" startup prompt', hub_bin, '${conv}'}, args = {{name = 'conv', required = true}}}}})

local function spawn(conv)
  return alice:hub('hub.spawn', {profile = 'reporter', key = 'spawn-' .. conv, args = {conv = conv}})
end

-- pane is the one pane with the spawn's label, or nil.
local function pane(sess, spawn_id)
  local r = rex.call('session.list_blocks', {session_id = sess})
  local found
  for _, b in ipairs(r and r.blocks or {}) do
    if b.label == 'lazyrex-spawn:' .. spawn_id then
      if found then error('two panes with the label of ' .. spawn_id, 0) end
      found = b.block_id
    end
  end
  return found
end

local function prompt(sess, block)
  local _, err = rex.call('block.method', {session_id = sess, block_id = block, method = 'write', args = {data = 'go\r'}})
  if err ~= nil then error('write: ' .. tostring(err), 0) end
end

-- lost spawns with a lost create answer: the spawn is pending, and its pane exists.
local function lost(conv)
  local id = fault({point = 'spawn-create', mode = 'lose-answer', op = 'spawn-' .. conv})
  local s = spawn(conv)
  expect(s.status, 'the outcome is unknown').to_be('pending')
  expect(fault_fired(id)).to_be(true)
  local block = H.wait('the pane of ' .. conv, function() return pane(s.session_id, s.spawn_id) end)
  return s, block
end

local function adopted(s, block, conv)
  local again = H.wait('the spawn opened', function()
    local r = spawn(conv)
    return r.status == 'open' and r
  end)
  expect(again.spawn_id).to_be(s.spawn_id)
  expect(again.block_id, 'the labeled pane').to_be(block)
end

test('a lost create answer: the report from the labeled pane opens the spawn', function()
  local s, block = lost('adopt-a')
  rex.sleep(0.5)
  expect(spawn('adopt-a').status, 'no report yet: still pending').to_be('pending')
  prompt(s.session_id, block)
  adopted(s, block, 'adopt-a')
end)

test('a hub crash after a lost create answer: the report to the new hub opens the spawn', function()
  local s, block = lost('adopt-b')
  hub:restart({crash = true})
  expect(spawn('adopt-b').status, 'the new hub does not adopt by the label').to_be('pending')
  prompt(s.session_id, block)
  adopted(s, block, 'adopt-b')
end)

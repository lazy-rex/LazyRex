-- Agent restart (hub.admin.restart; docs/hub.md, "Agent restart"). A fake agent CLI
-- (../fake-agent.sh) stops on one Ctrl-C (as Codex), on two (as Claude), only on SIGTERM, or only on
-- SIGKILL. Its configured program is a symlink in the lab: the hub compares the pane's exec path
-- (Rex invoked_path) with it, never argv0 and never the symlink's current target.

local alice = agent('alice')
local fake = H.args.file:match('^(.*)/tests/[^/]*$') .. '/fake-agent.sh'
local lab = H.args.lab
local bin = lab .. '/bin'

local function profile(id, kind, stop, program, when)
  return {id = id, name = id, kind = 'agent', cwd = lab,
    command = {program or (bin .. '/agent'), fake, kind, '${conv}', 'startup', when or 'start', '0', stop}, args = {{name = 'conv', required = true}},
    resume = {'/bin/bash', fake, kind, '${conversation_id}', 'resume', kind == 'claude' and 'start' or 'prompt', '0',
      stop == 'fgchild' and 'int' or stop}}
end

-- A forged argv0: the pane runs /bin/sh (its exec path) under the configured program's name.
local forged = {id = 'forged', name = 'forged', kind = 'agent', cwd = lab,
  command = {bin .. '/agent', '-c', 'exec -a "$0" /bin/sh "$@"', bin .. '/agent', fake, 'codex', '${conv}', 'startup', 'start'},
  args = {{name = 'conv', required = true}},
  resume = {'/bin/bash', fake, 'codex', '${conversation_id}', 'resume', 'prompt'}}

-- shell runs a script once in the lab directory; run_shell waits for it.
local function shell(id, script)
  return {id = id, name = id, kind = 'agent', cwd = lab,
    command = {'/bin/sh', '-c', script .. ' && touch "$PWD/' .. id .. '.done"; sleep 600'}}
end

write_profiles({profiles = {profile('codexlike', 'codex', 'int'), profile('claudelike', 'claude', 'int2'),
  profile('noint', 'codex', 'term'), profile('noterm', 'codex', 'kill'), profile('fgchild', 'codex', 'fgchild'), profile('versioned', 'codex', 'int', bin .. '/agent2'),
  profile('late', 'codex', 'int', bin .. '/agent3', 'prompt'), forged,
  shell('setup', 'mkdir -p "$PWD/bin" && for a in agent agent2 agent3; do ln -sfn /bin/bash "$PWD/bin/$a"; done'),
  shell('relink', 'ln -sfn /bin/dash "$PWD/bin/agent2"'),
  shell('relink3', 'ln -sfn /bin/dash "$PWD/bin/agent3"'),
  shell('back3', 'ln -sfn /bin/bash "$PWD/bin/agent3"')}})

local function exists(path)
  local f = io.open(path)
  if f then f:close() end
  return f ~= nil
end

local function run_shell(prof)
  alice:hub('hub.spawn', {profile = prof, key = prof})
  H.wait(prof, function() return exists(lab .. '/' .. prof .. '.done') end, 20)
end

run_shell('setup')

local function log(conv)
  local f = io.open(lab .. '/fake-' .. conv .. '.log')
  if not f then return '' end
  local s = f:read('*a')
  f:close()
  return s
end

local function identity(id)
  for _, i in ipairs(H.hub('hub.identity.list', {}).identities or {}) do
    if i.identity == id then return i end
  end
end

local function started(prof, kind, conv)
  local s = alice:hub('hub.spawn', {profile = prof, key = 'spawn-' .. conv, args = {conv = conv}})
  local id = kind .. ':' .. conv
  H.wait('the identity of ' .. conv, function()
    local i = identity(id)
    return i and i.state == 'current' and i.block_id == s.block_id
  end)
  return id, s.block_id, s.session_id
end

local function pane_child(sess, block)
  local r = rex.call('block.method', {session_id = sess, block_id = block, method = 'process', args = {}})
  return r and r.child
end

local function write_block(sess, block, data)
  local _, err = rex.call('block.method', {session_id = sess, block_id = block, method = 'write', args = {data = data}})
  if err ~= nil then error('write: ' .. tostring(err), 0) end
end

local function restart(args)
  return H.hub('hub.admin.restart', args)
end

local function target(r, id)
  for _, t in ipairs(r.targets) do if t.agent == id then return t end end
end

local function finished(key, args)
  args.key = key
  return H.wait('operation ' .. key, function()
    local r = restart(args)
    return r.done and r
  end, 60)
end

-- restarted checks the common outcome: the old program is gone, and the identity is current in the
-- new pane of the target.
local function restarted(r, id, old, sess)
  local t = target(r, id)
  expect(t.phase).to_be('rebound')
  expect(t.block_id ~= old).to_be(true)
  expect(identity(id).block_id).to_be(t.block_id)
  expect(pane_child(sess, old)).to_be(nil)
  return t
end

test('a Codex-like agent: one Ctrl-C stops it, then it resumes in a new pane', function()
  local id, old, sess = started('codexlike', 'codex', 'k1')
  restarted(finished('x-k1', {agent = id}), id, old, sess)
  expect(log('k1')).not_to_contain('signal: TERM')
  -- The resumed agent got its first prompt.
  expect(log('k1')).to_match('messaging_read')
end)

test('a Claude-like agent: two Ctrl-C stop it, no signal', function()
  local id, old, sess = started('claudelike', 'claude', 'k2')
  restarted(finished('x-k2', {agent = id}), id, old, sess)
  expect(log('k2')).not_to_contain('signal: TERM')
end)

test('an agent that ignores Ctrl-C gets SIGTERM', function()
  local id, old, sess = started('noint', 'codex', 'k3')
  restarted(finished('x-k3', {agent = id}), id, old, sess)
  expect(log('k3')).to_contain('signal: TERM')
end)

test('an agent that ignores Ctrl-C and SIGTERM gets SIGKILL', function()
  local id, old, sess = started('noterm', 'codex', 'k4')
  restarted(finished('x-k4', {agent = id}), id, old, sess)
end)

test('no exit keys when another process is in the foreground', function()
  local id, old, sess = started('fgchild', 'codex', 'k5')
  restarted(finished('x-k5', {agent = id}), id, old, sess)
  expect(log('k5')).not_to_contain('signal: INT')
end)

test('a moved symlink (a version update) does not change the exec path: the restart runs', function()
  local id, old, sess = started('versioned', 'codex', 'k6')
  run_shell('relink')
  expect(pane_child(sess, old).invoked_path).to_be(nil)
  -- A later report of the same program (its exec path is null now) keeps the recorded values.
  write_block(sess, old, 'hello\r')
  write_block(sess, old, 'again\r')
  H.wait('the second report', function() return log('k6'):find('prompt: hello', 1, true) end)
  rex.sleep(2)
  restarted(finished('x-k6', {agent = id}), id, old, sess)
end)

test('a forged argv0 is refused, and nothing is sent', function()
  local id, old, sess = started('forged', 'codex', 'k7')
  local child = pane_child(sess, old)
  expect(child.argv0).to_be(bin .. '/agent')
  local t = target(restart({key = 'x-k7', agent = id}), id)
  expect(t.phase).to_be('refused')
  expect(t.detail).to_match('not the configured program')
  rex.sleep(3)
  expect(pane_child(sess, old).pid).to_be(child.pid)
  expect(identity(id).state).to_be('current')
end)

test('refusals: a resume with a restart key is another request; busy', function()
  local id, old, sess = started('noterm', 'codex', 'k8')
  local r = restart({key = 'x-k8', agent = id})
  expect(target(r, id).phase).to_be('restart_admitted')
  H.expect_error(function() H.hub('hub.admin.resume', {key = 'x-k8', agent = id}) end, 'different request')
  expect(target(restart({key = 'x-k8b', agent = id}), id).detail).to_match('busy')
  restarted(finished('x-k8', {agent = id}), id, old, sess)
end)

test('kind restarts every current agent of that kind', function()
  local a, olda, sess = started('codexlike', 'codex', 'k9')
  local b, oldb = started('noint', 'codex', 'k10')
  local r = finished('x-kind', {kind = 'codex'})
  restarted(r, a, olda, sess)
  restarted(r, b, oldb, sess)
end)

test('an exec to another path in the same pid is refused', function()
  local id, old, sess = started('codexlike', 'codex', 'k11')
  local pid = pane_child(sess, old).pid
  write_block(sess, old, 'hello\r')
  write_block(sess, old, 'exec\r')
  H.wait('the exec', function() local c = pane_child(sess, old); return c and c.path == '/bin/dash' end)
  expect(pane_child(sess, old).pid).to_be(pid)
  local t = target(restart({key = 'x-k11', agent = id}), id)
  expect(t.phase).to_be('refused')
  expect(t.detail).to_match('executable changed')
end)

test('nothing recorded (the symlink moved before the report): refused while Rex has no exec path, then allowed', function()
  local s = alice:hub('hub.spawn', {profile = 'late', key = 'spawn-k12', args = {conv = 'k12'}})
  local id = 'codex:k12'
  run_shell('relink3')
  write_block(s.session_id, s.block_id, 'hello\r')
  H.wait('the report of k12', function() local i = identity(id); return i and i.state == 'current' end)
  local t = target(restart({key = 'x-k12', agent = id}), id)
  expect(t.phase).to_be('refused')
  expect(t.detail).to_match('cannot be verified')
  -- The symlink resolves to the executable again: the live exec path is complete.
  run_shell('back3')
  restarted(finished('x-k12b', {agent = id}), id, s.block_id, s.session_id)
end)

-- Agent restart (hub.admin.restart; docs/hub.md, "Agent restart"). The fake agent CLI is a program
-- of its own (`lazyrex-hub testagent cli`, hub/cmd/hub/testcli.go), run through a lab symlink named
-- codex or claude: v1 restarts only a direct CLI, never a pane shell. It stops on one Ctrl-C (as
-- Codex), on two (as Claude), only on SIGTERM, or only on SIGKILL. The hub compares the exec path that
-- it recorded at the identity report with the configured program, never argv0.

local alice = agent('alice')
local lab = H.args.lab
local hub_bin = H.args.hub_bin

-- dir/name is a symlink to the hub binary: dir separates the tests that move a symlink. A resume
-- runs through bin/, which no test moves.
local function profile(id, kind, stop, dir, when)
  local prog = lab .. '/' .. (dir or 'bin') .. '/' .. kind
  return {id = id, name = id, kind = 'agent', cwd = lab,
    command = {prog, 'testagent', 'cli', kind, '${conv}', 'startup', when or 'start', stop}, args = {{name = 'conv', required = true}},
    resume = {lab .. '/bin/' .. kind, 'testagent', 'cli', kind, '${conversation_id}', 'resume', kind == 'claude' and 'start' or 'prompt',
      stop == 'fgchild' and 'int' or stop}}
end

-- A forged argv0: the pane runs the hub binary under the name of a codex symlink. exec -a is a bash
-- feature (on Linux /bin/sh can be dash, which has no -a).
local forged = {id = 'forged', name = 'forged', kind = 'agent', cwd = lab,
  command = {'/bin/bash', '-c', 'exec -a "$0" "$1" testagent cli codex "$2" startup start', lab .. '/bin/codex', hub_bin, '${conv}'},
  args = {{name = 'conv', required = true}}, resume = {'/bin/sh', '${conversation_id}'}}

-- A pane shell: its program is the configured /bin/bash, but it is not an agent CLI.
local fake_sh = H.args.file:match('^(.*)/tests/[^/]*$') .. '/fake-agent.sh'
local shellpane = {id = 'shellpane', name = 'shellpane', kind = 'agent', cwd = lab,
  command = {'/bin/bash', fake_sh, 'codex', '${conv}', 'startup', 'start'}, args = {{name = 'conv', required = true}},
  resume = {'/bin/bash', fake_sh, 'codex', '${conversation_id}', 'resume', 'prompt'}}

-- shell runs a script once in the lab directory; run_shell waits for it.
local function shell(id, script)
  return {id = id, name = id, kind = 'agent', cwd = lab,
    command = {'/bin/sh', '-c', script .. ' && touch "$PWD/' .. id .. '.done"; sleep 600'}}
end

local link = 'ln -sfn "' .. hub_bin .. '" '
write_profiles({profiles = {profile('codexlike', 'codex', 'int'), profile('claudelike', 'claude', 'int2'),
  profile('noint', 'codex', 'term'), profile('noterm', 'codex', 'kill'), profile('fgchild', 'codex', 'fgchild'),
  profile('versioned', 'codex', 'int', 'ver'), profile('late', 'codex', 'int', 'late', 'prompt'),
  profile('stale', 'codex', 'int', 'stale'), forged, shellpane,
  shell('setup', 'for d in bin ver late stale; do mkdir -p "$PWD/$d" && ' .. link .. '"$PWD/$d/codex" && ' .. link .. '"$PWD/$d/claude"; done'),
  shell('relink', 'ln -sfn /bin/sleep "$PWD/ver/codex"'),
  shell('relink3', 'ln -sfn /bin/sleep "$PWD/late/codex"'),
  shell('back3', link .. '"$PWD/late/codex"'),
  shell('relinks', 'ln -sfn /bin/sleep "$PWD/stale/codex"')}})

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

-- exec_sleep makes the fake exec /bin/sleep in its pid, and waits for the same pid with a new path
-- whose base name is sleep. Rex reports the resolved path: /usr/bin/sleep where /bin is a link to
-- /usr/bin (Linux with merged /usr), so the wait names no directory.
local function exec_sleep(sess, block)
  local before = pane_child(sess, block)
  write_block(sess, block, 'hello\r')
  write_block(sess, block, 'exec\r')
  H.wait('the exec', function()
    local c = pane_child(sess, block)
    return c and c.pid == before.pid and c.path ~= before.path and c.path:match('/sleep$') ~= nil
  end)
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

-- restarted checks the common outcome: the old program is gone, the identity is current in the new
-- pane of the target, and the reply keeps the stop's outcome after the resume.
local function restarted(r, id, old, sess, stop)
  local t = target(r, id)
  expect(t.phase).to_be('rebound')
  expect(t.stop).to_be(stop)
  expect(t.block_id ~= old).to_be(true)
  expect(identity(id).block_id).to_be(t.block_id)
  expect(pane_child(sess, old)).to_be(nil)
  return t
end

test('kind restarts every current agent of that kind', function()
  local a, olda, sess = started('codexlike', 'codex', 'k9')
  local b, oldb = started('noint', 'codex', 'k10')
  local r = finished('x-kind', {kind = 'codex'})
  restarted(r, a, olda, sess, 'exited after the exit keys')
  restarted(r, b, oldb, sess, 'exited after SIGTERM')
end)

test('a Codex-like agent: one Ctrl-C stops it, then it resumes in a new pane', function()
  local id, old, sess = started('codexlike', 'codex', 'k1')
  restarted(finished('x-k1', {agent = id}), id, old, sess, 'exited after the exit keys')
  expect(log('k1')).not_to_contain('signal: TERM')
  -- The resumed agent got its first prompt.
  expect(log('k1')).to_contain('messaging_read')
  -- A retry of the key replays the same outcome.
  expect(target(restart({key = 'x-k1', agent = id}), id).stop).to_be('exited after the exit keys')
end)

test('a Claude-like agent: two Ctrl-C stop it, no signal', function()
  local id, old, sess = started('claudelike', 'claude', 'k2')
  restarted(finished('x-k2', {agent = id}), id, old, sess, 'exited after the exit keys')
  expect(log('k2')).not_to_contain('signal: TERM')
end)

test('an agent that ignores Ctrl-C gets SIGTERM', function()
  local id, old, sess = started('noint', 'codex', 'k3')
  restarted(finished('x-k3', {agent = id}), id, old, sess, 'exited after SIGTERM')
  expect(log('k3')).to_contain('signal: TERM')
end)

test('an agent that ignores Ctrl-C and SIGTERM gets SIGKILL', function()
  local id, old, sess = started('noterm', 'codex', 'k4')
  restarted(finished('x-k4', {agent = id}), id, old, sess, 'exited after SIGKILL')
end)

test('no exit keys when another process is in the foreground', function()
  local id, old, sess = started('fgchild', 'codex', 'k5')
  -- The fake reports before its child takes the foreground (testcli.go): a restart before that
  -- sees the agent in the foreground and sends the exit keys.
  H.wait('the child in the foreground', function()
    local r = rex.call('block.method', {session_id = sess, block_id = old, method = 'process', args = {}})
    return r and r.child and r.foreground and r.foreground.pid ~= r.child.pid
  end)
  restarted(finished('x-k5', {agent = id}), id, old, sess, 'exited after SIGTERM')
  expect(log('k5')).not_to_contain('signal: INT')
end)

test('a moved symlink (a version update) does not change the recorded exec path: the restart runs', function()
  local id, old, sess = started('versioned', 'codex', 'k6')
  run_shell('relink')
  expect(pane_child(sess, old).invoked_path).to_be(nil)
  -- A later report of the same program (its exec path is null now) keeps the recorded values.
  write_block(sess, old, 'hello\r')
  write_block(sess, old, 'again\r')
  H.wait('the first line', function() return log('k6'):find('prompt: hello', 1, true) end)
  rex.sleep(2)
  restarted(finished('x-k6', {agent = id}), id, old, sess, 'exited after the exit keys')
end)

test('a stale report does not replace the recorded values', function()
  local id, old, sess = started('stale', 'codex', 'k13')
  run_shell('relinks')
  write_block(sess, old, 'hello\r')
  write_block(sess, old, 'stale\r')
  H.wait('the stale report', function()
    for _, e in ipairs(H.hub('hub.identity.list', {log = 50}).log or {}) do
      if e.identity == id and e.event == 'stale_report' then return true end
    end
  end, 15)
  restarted(finished('x-k13', {agent = id}), id, old, sess, 'exited after the exit keys')
end)

test('a forged argv0 is refused, and nothing is sent', function()
  local id, old, sess = started('forged', 'codex', 'k7')
  local child = pane_child(sess, old)
  expect(child.argv0).to_be(lab .. '/bin/codex')
  local t = target(restart({key = 'x-k7', agent = id}), id)
  expect(t.phase).to_be('refused')
  expect(t.detail).to_match('not the configured program')
  rex.sleep(3)
  expect(pane_child(sess, old).pid).to_be(child.pid)
  expect(identity(id).state).to_be('current')
end)

test('a pane shell is refused: it is not the agent CLI', function()
  local id, old, sess = started('shellpane', 'codex', 'k15')
  local t = target(restart({key = 'x-k15', agent = id}), id)
  expect(t.phase).to_be('refused')
  expect(t.detail).to_match('not the codex CLI')
  expect(identity(id).state).to_be('current')
end)

test('an exec to another path in the same pid is refused', function()
  local id, old, sess = started('codexlike', 'codex', 'k11')
  local pid = pane_child(sess, old).pid
  exec_sleep(sess, old)
  expect(pane_child(sess, old).pid).to_be(pid)
  local t = target(restart({key = 'x-k11', agent = id}), id)
  expect(t.phase).to_be('refused')
  expect(t.detail).to_match('executable changed')
end)

test('a check that fails during the stop: failed, the reply says not stopped', function()
  local id, old, sess = started('noterm', 'codex', 'k14')
  restart({key = 'x-k14', agent = id})
  -- The fake acknowledges the exit key (it logs it and stays). A Ctrl-C flushes a pending input line,
  -- so the lines go only after it.
  H.wait('the exit key', function() return log('k14'):find('signal: INT', 1, true) end)
  local pid = pane_child(sess, old).pid
  exec_sleep(sess, old)
  expect(pane_child(sess, old).pid).to_be(pid)
  local t = target(finished('x-k14', {agent = id}), id)
  expect(t.phase).to_be('failed')
  expect(t.stop).to_match('^not stopped: ')
  expect(t.stop).to_match('executable changed')
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
  restarted(finished('x-k12b', {agent = id}), id, s.block_id, s.session_id, 'exited after the exit keys')
end)

test('refusals: a resume with a restart key is another request; busy', function()
  local id, old, sess = started('noterm', 'codex', 'k8')
  expect(target(restart({key = 'x-k8', agent = id}), id).phase).to_be('restart_admitted')
  H.expect_error(function() H.hub('hub.admin.resume', {key = 'x-k8', agent = id}) end, 'different request')
  expect(target(restart({key = 'x-k8b', agent = id}), id).detail).to_match('busy')
  restarted(finished('x-k8', {agent = id}), id, old, sess, 'exited after SIGKILL')
end)

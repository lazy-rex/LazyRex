-- Hub Status for an agent pane with no current conversation identity (docs/hub.md, "Hub controls").
-- A real spawn of an agent profile whose CLI does not run its hook: hub.workers says identity
-- "none", and `lazyrex-hub status` shows the advisory line. Then the CLI runs the real hook
-- command (`lazyrex-hub hook claude`) from its pane: identity "current", and the line goes away.

local function worker(a, id)
  for _, w in ipairs(a:hub('hub.workers', {}).workers) do if w.spawn_id == id then return w end end
end

-- hub_status runs Hub Status for the test hub's config and returns its output and exit code.
local function hub_status()
  local r = assert(rex.exec{'sh', '-c', '"$0" status -config "$1" 2>&1; echo "exit=$?"', H.args.hub_bin, H.args.lab .. '/hub.json'})
  local out = r.stdout or ''
  return out, tonumber(out:match('exit=(%d+)%s*$'))
end

test('a silent agent pane shows in Hub Status until its hook reports', function()
  local flag = H.args.lab .. '/silent-report-now'
  -- The fake agent CLI waits for the flag file, then runs the real hook with a SessionStart event.
  local script = 'while [ ! -e "$2" ]; do sleep 0.1; done; ' ..
    'printf \'%s\' \'{"hook_event_name":"SessionStart","session_id":"silent-1","source":"startup"}\' | "$1" hook claude; ' ..
    'exec sleep 600'
  write_profiles({profiles = {{id = 'silent', name = 'Silent agent', kind = 'agent',
    command = {'/bin/sh', '-c', script, 'sh', H.args.hub_bin, flag}}}})
  local alice = agent('alice')
  local s = alice:hub('hub.spawn', {profile = 'silent', key = 'silent-1', title = 'Silent'})
  expect(s.agent_id ~= nil, 'an agent profile gets an agent ID').to_be(true)
  local w = wait('the silent agent runs', function()
    local w = worker(alice, s.spawn_id)
    return w and w.state == 'running' and w
  end, 20)
  expect(w.identity).to_be('none')

  local out, code = hub_status()
  expect(code, out).to_be(0)
  expect(out).to_match('running: hub pid')
  expect(out).to_contain('agent Silent (pane ' .. w.block_id .. '): no current conversation identity yet.')

  -- The real hook runs from the pane: the identity becomes current, and the line goes away.
  rex.exec{'touch', flag}
  wait('the hook report', function() return worker(alice, s.spawn_id).identity == 'current' end, 20)
  out, code = hub_status()
  expect(code, out).to_be(0)
  expect(out).not_to_contain('no current conversation identity')

  -- An accessory has no field.
  local sh = alice:hub('hub.spawn', {profile = 'shell', key = 'shell-1'})
  local acc = wait('the shell runs', function()
    local w = worker(alice, sh.spawn_id)
    return w and w.state == 'running' and w
  end, 20)
  expect(acc.identity).to_be(nil)
  alice:hub('hub.stop', {spawn_id = sh.spawn_id})
  alice:hub('hub.stop', {spawn_id = s.spawn_id})
end)

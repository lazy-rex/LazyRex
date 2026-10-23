-- The identity reconcile fields of hub.workers (docs/hub.md, hub.workers): identity_id and
-- identity_seen of a real agent spawn, through real hook reports from its pane, on both faces:
-- an agent calls hub.workers {spawn_id}; a plugin render calls hub.workers {spawn_id, render} (the
-- fixture route /lazy/workers-<spawn_id>), and a tracked render renders again on the workers change.

local marker = tostring(os.time())
local flags = H.args.lab .. '/fields-' .. marker

-- The fake agent CLI runs the real hook (`lazyrex-hub hook claude`) from its pane when a flag file
-- appears: r1 a startup report, r1b the same report again, r2 a /clear to a new conversation. On
-- the flag end it exits.
local script = [[
w() { while [ ! -e "$2/$1" ]; do sleep 0.1; done; }
hook() { printf '{"hook_event_name":"SessionStart","session_id":"%s","source":"%s"}' "$1" "$2" | "$H" hook claude; }
H="$1"; d="$2"; m="$3"
w r1 "$d"; hook "f1-$m" startup
w r1b "$d"; hook "f1-$m" startup
w r2 "$d"; hook "f2-$m" clear
w end "$d"; exit 0
]]

local function flag(name) rex.exec{'sh', '-c', 'mkdir -p "$0" && touch "$0/$1"', flags, name} end

local function worker(a, id) return a:hub('hub.workers', {spawn_id = id}).workers[1] end

local function count(v)
  for _, c in ipairs(renders()) do
    if c.view_id == v.id then return c.attempts end
  end
  return 0
end

-- settle waits until the view starts no render for one second, and returns its count.
local function settle(v)
  local last = -1
  return wait('the renders to settle', function()
    local n = count(v)
    if n == last then return n end
    last = n
    rex.sleep(1)
    return nil
  end, 20)
end

test('identity_id and identity_seen follow real reports, a /clear, a hub restart and the exit', function()
  write_profiles({profiles = {{id = 'fields', name = 'Fields agent', kind = 'agent',
    command = {'/bin/sh', '-c', script, 'sh', H.args.hub_bin, flags, marker}}}})
  local alice = agent('alice')
  local s = alice:hub('hub.spawn', {profile = 'fields', key = 'fields-' .. marker, title = 'Fields'})
  local w = wait('the agent runs', function()
    local w = worker(alice, s.spawn_id)
    return w and w.state == 'running' and w
  end, 20)
  expect(w.identity, 'before any report').to_be('none')
  expect(w.identity_id, 'before any report').to_be(nil)
  expect(w.identity_seen, 'before any report').to_be(false)

  -- The agent face: render is not an argument there; not_found for an unknown spawn.
  expect_error(function() alice:hub('hub.workers', {spawn_id = s.spawn_id, render = 'x'}) end, 'render')
  expect_error(function() alice:hub('hub.workers', {spawn_id = 'spawn-none-' .. marker}) end, 'not_found')

  -- The plugin face: a render reads the worker with its render ID.
  local v = user:open('lazy/workers-' .. s.spawn_id)
  expect(node(v:render(), 'lazy').text, 'the plugin render before any report').to_be('none false')
  local n0 = settle(v)

  local first = 'claude:f1-' .. marker
  flag('r1')
  w = wait('the first report', function()
    local w = worker(alice, s.spawn_id)
    return w.identity_id and w
  end, 20)
  expect(w.identity_id).to_be(first)
  expect(w.identity_seen).to_be(true)
  expect(w.identity).to_be('current')
  -- The workers change renders the tracked view again, with no other cause.
  local n1 = wait('the view renders on the workers change', function()
    local n = count(v)
    return n > n0 and n
  end, 20)
  n1 = settle(v)

  -- The same report again: unchanged, so no workers change and no render.
  flag('r1b')
  rex.sleep(3) -- the fake agent polls its flags every 0.1 s; its hook ends within 3 s (hook.go)
  expect(settle(v), 'no render after an unchanged report').to_be(n1)
  expect(worker(alice, s.spawn_id).identity_id, 'after the unchanged report').to_be(first)

  local second = 'claude:f2-' .. marker
  flag('r2')
  w = wait('the /clear report', function()
    local w = worker(alice, s.spawn_id)
    return w.identity_id == second and w
  end, 20)
  expect(w.identity_seen, 'after a /clear').to_be(true)
  wait('the view renders on the /clear', function() return count(v) > n1 end, 20)
  expect(node(v:render(), 'lazy').text, 'the plugin render after the /clear').to_be(second .. ' true')

  -- A hub restart: identity_id from the stored current identity, identity_seen from the log.
  H.restart_hub()
  w = worker(alice, s.spawn_id)
  expect(w.identity_id, 'after a hub restart').to_be(second)
  expect(w.identity_seen, 'after a hub restart').to_be(true)

  -- The agent process exits. The lab records the state this gives (the fake agent is the pane's
  -- direct child; a hub.spawn pane closes on exit unless the profile keeps it).
  flag('end')
  local outcome = wait('the exit is visible', function()
    local ok, r = pcall(worker, alice, s.spawn_id)
    if not ok then
      local text = type(r) == 'table' and tostring(r.message) or tostring(r)
      return text:match('not_found') and {gone = text} or nil
    end
    if r.identity_id == nil then return {worker = r} end
    return nil
  end, 20)
  if outcome.gone then
    print('OBSERVED after the exit: hub.workers {spawn_id} -> ' .. outcome.gone)
  else
    print('OBSERVED after the exit: state ' .. tostring(outcome.worker.state) .. ', identity_seen ' .. tostring(outcome.worker.identity_seen))
    expect(outcome.worker.identity_seen, 'after the exit').to_be(true)
    alice:hub('hub.stop', {spawn_id = s.spawn_id})
  end
end)

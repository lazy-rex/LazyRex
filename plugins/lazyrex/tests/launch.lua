-- Launching workers.
-- Both paths: the user submits the Launch Profile… form (tool = 'hub.spawn'); an agent calls
-- hub.spawn. The task profile writes its two args to LAB/home/task-args. The file stops the
-- launcher stub first: the home plugin registers Launch Profile… only while no other plugin offers
-- a launch (a palette command whose action is hub.spawn).

local codex = agent('codex', {kind = 'codex', title = 'Codex'})
local args_file = H.args.lab .. '/home/task-args'

H.hub('hub.admin.stop', {plugin = 'launcher'})
wait('the home plugin registers Launch Profile…', function()
  for _, c in ipairs(H.hub('hub.test.commands', {plugin = 'lazyrex'}).commands or {}) do
    if c.id == 'launch' then return true end
  end
end)

local function home() return user:panel('launch') end

test('the form has one launch form per profile, with the profile args', function()
  local tree = home():render()
  expect(tree.title).to_be('Launch Profile')
  local task = H.node(tree, 'launch/task')
  expect(task.label).to_be('Launch Task')
  expect(task.fields[1]).to_equal({id = 'title', label = 'Tab title (optional)', kind = 'text', required = false, value = ''})
  expect(task.fields[2]).to_equal({id = 'arg/task', label = 'Task', kind = 'multiline', required = true, value = 'Inspect changes'})
  expect(task.fields[3].value).to_be('review')
  expect(task.submit_action.tool).to_be('hub.spawn')
  expect(#H.node(tree, 'launch/shell').fields, 'no args: only the title').to_be(1)
  expect(H.node(tree, 'cancel').action.name).to_be('$cancel')
end)
local function find(list, id)
  for _, x in ipairs(list or {}) do if x.spawn_id == id then return x end end
end
local function workers() return codex:call('hub.workers', {}).workers or {} end
local function read_args()
  return wait('the task worker writes its args', function()
    local f = io.open(args_file, 'r')
    if not f then return nil end
    local s = f:read('*a')
    f:close()
    return s
  end)
end

local user_spawn
test('user path: a launch starts the worker with its title and shows its tab', function()
  local op = home():action('launch/shell', 'launch', {values = {title = 'Test'}}, 'launch-shell')
  expect(op.status).to_be('succeeded')
  expect(op.result.status).to_be('open')
  user_spawn = op.result.spawn_id
  local w = find(workers(), user_spawn)
  expect(w.title).to_be('Test')
  expect(w.parent, 'the plugin launched it for the user').to_be('user')
  expect(H.node(user:panel('agents'):render(), 'navigation_notice'), 'the hub showed the tab').to_be(nil)
end)

test('agent path: hub.spawn starts the same profile', function()
  local r = codex:call('hub.spawn', {profile = 'shell', key = 'launch-shell-agent', title = 'Test'})
  expect(r.status).to_be('open')
  local w = find(workers(), r.spawn_id)
  expect(w.title).to_be('Test')
  expect(w.parent).to_be(codex.id)
end)

local named
test('named inputs reach the worker as profile args, on both paths', function()
  os.remove(args_file)
  local op = home():action('launch/task', 'launch',
    {values = {title = 'Task tab', ['arg/task'] = 'Read code\nExplain findings', ['arg/mode'] = 'review'}}, 'named-args')
  expect(op.status).to_be('succeeded')
  named = op.result
  expect(read_args()).to_be('Read code\nExplain findings|review')
  os.remove(args_file)
  codex:call('hub.spawn', {profile = 'task', key = 'named-args-agent', title = 'Task tab',
    args = {task = 'Read code\nExplain findings', mode = 'review'}})
  expect(read_args()).to_be('Read code\nExplain findings|review')
end)

test('user path: the hub retry after a profile removal rebuilds the same named inputs', function()
  -- The first attempt commits the spawn and loses its reply. A gate holds the hub's retry while
  -- the test removes the profile. After the release the plugin must send the same request
  -- (args from the form values, not from the profile), or the hub refuses it.
  local lost = fault({point = 'action', action = 'hub.spawn', when = 'after', mode = 'lose-reply'})
  local gate = fault({point = 'action', action = 'hub.spawn', when = 'before', mode = 'gate', nth = 2})
  local before = #workers()
  os.remove(args_file)
  local v = home()
  local _, key = v:admit('action', {node_id = 'launch/task', action = 'launch', revision = v.revision,
    input = {values = {title = 'Rebuilt', ['arg/task'] = 'Rebuild\nthe inputs', ['arg/mode'] = 'retry'}}})
  wait('the retry reaches the gate', function() return fault_fired(gate) end)
  expect(fault_fired(lost)).to_be(true)
  expect(read_args(), 'the first attempt started the worker').to_be('Rebuild\nthe inputs|retry')
  write_profiles({profiles = {{id = 'other', name = 'Other', kind = 'accessory', command = {'/bin/sh', '-c', 'exec sleep 600'}}}})
  expect(H.node(home():render(), 'launch/task'), 'the profile is gone').to_be(nil)
  fault_release(gate)
  local op = v:settle(key)
  expect(op.status).to_be('succeeded')
  expect(op.result.status).to_be('open')
  expect(#workers(), 'the retry replayed the first spawn').to_be(before + 1)
end)

-- hub.spawn replays a used key before it loads profiles, so a retry survives a profile removal.
test('agent path: a retry after the profile was removed replays the first launch', function()
  write_profiles({profiles = {{id = 'other', name = 'Other', kind = 'accessory', command = {'/bin/sh', '-c', 'exec sleep 600'}}}})
  local before = #workers()
  local first = find(workers(), named.spawn_id)
  expect(first, 'the user launch is still listed').to_be_truthy()
  local again = codex:call('hub.spawn', {profile = 'task', key = 'named-args-agent', title = 'Task tab',
    args = {task = 'Read code\nExplain findings', mode = 'review'}})
  expect(again.status).to_be('open')
  expect(#workers(), 'no new worker').to_be(before)
  expect_error(function() codex:call('hub.spawn', {profile = 'task', key = 'named-args-new', title = 'Task tab'}) end, 'no profile')
end)

test('a changed request under a used key is refused, on both paths', function()
  -- The user path: the hub refuses a view operation key that is used with other input.
  expect_error(function() home():action('launch/shell', 'launch', {values = {title = 'Changed'}}, 'launch-shell') end, 'different arguments')
  expect_error(function() codex:call('hub.spawn', {profile = 'shell', key = 'launch-shell-agent', title = 'Changed'}) end, 'different request')
end)

test('launch inputs must be text', function()
  local v = home()
  local op = v:action('launch/shell', 'launch', {values = {title = 7}}, 'launch-bad-title')
  expect(op.status).to_be('failed')
  expect(op.error.message).to_match('title must be text')
  op = v:action('launch/shell', 'launch', {values = {title = '', ['arg/task'] = 42}}, 'launch-bad-arg')
  expect(op.status).to_be('failed')
  expect(op.error.message).to_match('must be text')
end)

test('a successful launch closes the form; a failed launch keeps it', function()
  local v = home()
  local op = v:action('launch/shell', 'launch', {values = {title = 'Closes'}}, 'launch-closes')
  expect(op.result.close_form).to_be(true)
  wait('the form closes', function()
    for _, w in ipairs(views()) do if w.view_id == v.id then return false end end
    return true
  end)
  fault({point = 'spawn-create', mode = 'refuse', op = 'launch-keeps'})
  local kept = home()
  local failed = kept:action('launch/shell', 'launch', {values = {title = 'Keeps'}}, 'launch-keeps')
  expect(failed.result.status).to_be('failed')
  expect(failed.result.close_form).to_be(nil)
  kept:render() -- still open
end)

test('a profile error keeps the form with its message', function()
  write_profiles('{"profiles": [')
  local tree = home():render()
  expect(H.node(tree, 'profiles_error').text).to_match('Profiles could not be loaded')
  expect(H.node(tree, 'profiles_error').text).to_match('profiles.json')
  expect(H.node(tree, 'launch/shell'), 'no launch form without profiles').to_be(nil)
  expect(H.node(user:panel('agents'):render(), 'workers'), 'profile errors keep the panel').to_be_truthy()
end)

test('without profile files only the builtin shell profile remains', function()
  write_profiles('{"profiles":[]}')
  local tree = home():render()
  expect(H.node(tree, 'launch/shell').label, 'the builtin shell').to_be('Launch Shell')
  expect(H.node(tree, 'launch/task')).to_be(nil)
  expect(H.node(tree, 'no_profiles'), 'the hub always has the builtin shell').to_be(nil)
  expect(H.node(tree, 'profiles_error')).to_be(nil)
end)

test('a launch whose navigation fails still reports the launch; the panel shows a notice', function()
  write_profiles('{"profiles":[]}')
  local id = fault({point = 'action', action = 'hub.navigate.worker', when = 'before', mode = 'fail'})
  local op = home():action('launch/shell', 'launch', {values = {title = 'No navigation'}}, 'nav-launch-fail')
  expect(fault_fired(id)).to_be(true)
  expect(op.status).to_be('succeeded')
  expect(op.result.status, 'a navigation failure does not report a failed launch').to_be('open')
  expect(H.node(user:panel('agents'):render(), 'navigation_notice').text).to_match('worker started')
  local again = home():action('launch/shell', 'launch', {values = {title = 'Navigated'}}, 'nav-launch-ok')
  expect(again.status).to_be('succeeded')
  expect(H.node(user:panel('agents'):render(), 'navigation_notice'), 'the next launch clears the notice').to_be(nil)
end)

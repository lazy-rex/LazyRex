-- The restart form (docs/hub.md, "Restart form"): the hub owns it (owner hub:restart). The test
-- opens it as the palette entry does (hub.admin.restart.form {kind, ctx}), renders it through a real
-- renderer pane, and submits through the hub's view path. The fake agent CLI is the one of
-- agent_restart.lua: a lab symlink named codex to the hub binary, stopped by one Ctrl-C.

local alice = agent('alice')
local lab = H.args.lab
local hub_bin = H.args.hub_bin
local owner = 'hub:restart'

write_profiles({profiles = {
  {id = 'codexlike', name = 'codexlike', kind = 'agent', cwd = lab,
    command = {lab .. '/bin/codex', 'testagent', 'cli', 'codex', '${conv}', 'startup', 'start', 'int'}, args = {{name = 'conv', required = true}},
    resume = {lab .. '/bin/codex', 'testagent', 'cli', 'codex', '${conversation_id}', 'resume', 'prompt', 'int'}},
  {id = 'setup', name = 'setup', kind = 'agent', cwd = lab,
    command = {'/bin/sh', '-c', 'mkdir -p "$PWD/bin" && ln -sfn "' .. hub_bin .. '" "$PWD/bin/codex" && touch "$PWD/setup.done"; sleep 600'}}}})

local function exists(path)
  local f = io.open(path)
  if f then f:close() end
  return f ~= nil
end

alice:hub('hub.spawn', {profile = 'setup', key = 'setup'})
H.wait('setup', function() return exists(lab .. '/setup.done') end, 20)

local function identity(id)
  for _, i in ipairs(H.hub('hub.identity.list', {}).identities or {}) do
    if i.identity == id then return i end
  end
end

local function started(conv)
  local s = alice:hub('hub.spawn', {profile = 'codexlike', key = 'spawn-' .. conv, args = {conv = conv}})
  local id = 'codex:' .. conv
  H.wait('the identity of ' .. conv, function()
    local i = identity(id)
    return i and i.state == 'current' and i.block_id == s.block_id
  end)
  return id, s.block_id, s.session_id
end

-- open runs the palette entry's action and returns the form's view. The form's view is new each
-- time: the views of an earlier form are closed first.
local function open(sess)
  H.hub('hub.admin.restart.form', {kind = 'codex', ctx = {client_id = H.self_id, session_id = sess}})
  return user:view('restart', owner, sess)
end

local function items(tree, id)
  local n = node(tree, id)
  return n and n.items or {}
end

local function item(tree, list, id)
  for _, i in ipairs(items(tree, list)) do if i.id == id then return i end end
end

local function code(op)
  return rex.json.encode(op.error or {})
end

-- done waits until the frozen form shows "Done." and returns that tree.
local function done(form)
  return H.wait('the restart to finish', function()
    local tree = form:render()
    if node(tree, 'state') and node(tree, 'state').text == 'Done.' then return tree end
  end, 60)
end

test('the form lists the snapshot and survives a reconcile pass', function()
  local id, block, sess = started('f1')
  local form = open(sess)
  local tree = form:render()
  expect(item(tree, 'targets', id) ~= nil).to_be(true)
  expect(node(tree, 'restart').action.name).to_be('restart@1')
  expect(node(tree, 'restart').action.enabled).to_be(true)
  -- A navigation runs a UI pass. The form is not a tab of a plugin: the pass must not retire it.
  user:open('home')
  rex.sleep(2)
  local still = false
  for _, w in ipairs(H.views(owner)) do if w.view_id == form.id then still = true end end
  expect(still).to_be(true)
  form:action('close', 'close', {})
end)

test('restart runs the snapshot; a retry after a lost reply replays; the form is frozen', function()
  local id, old, sess = started('f2')
  local form = open(sess)
  form:render()
  local op = form:action('restart', 'restart@1', {})
  expect(op.status).to_be('succeeded')
  local op_id = op.result.operation.op_id
  local tree = done(form)
  expect(item(tree, 'outcome', id).detail).to_contain('rebound')
  expect(identity(id).block_id ~= old).to_be(true)
  -- A lost reply: the renderer sends the same action under a new operation key. The hub finds the
  -- operation by its stored key and reads no target (they changed: the agent is in a new pane).
  local again = form:action('restart', 'restart@1', {})
  expect(again.status).to_be('succeeded')
  expect(again.result.operation.op_id).to_be(op_id)
  -- Frozen: no refresh, no new restart in the tree.
  local refresh = form:action('refresh', 'refresh', {})
  expect(refresh.status).to_be('failed')
  expect(code(refresh)).to_contain('invalid_state')
  expect(node(tree, 'refresh')).to_be(nil)
  expect(node(tree, 'restart')).to_be(nil)
  form:action('close', 'close', {})
end)

test('a change after the form opened refuses the whole batch; refresh takes a new snapshot', function()
  local a, olda, sess = started('f3')
  local b = started('f4')
  local form = open(sess)
  form:render()
  -- b moves to another pane behind the form's back.
  local r = H.wait('the restart of ' .. b, function()
    local x = H.hub('hub.admin.restart', {key = 'behind', agent = b})
    return x.done and x
  end, 60)
  expect(r.targets[1].phase).to_be('rebound')
  local tree = form:render()
  expect(item(tree, 'targets', b).detail).to_contain('moved to another pane')
  expect(node(tree, 'restart').action.enabled).to_be(false)
  -- A renderer that sends the restart anyway: refused, nothing restarted.
  local op = form:action('restart', 'restart@1', {})
  expect(op.status).to_be('failed')
  expect(code(op)).to_contain('conflict')
  expect(identity(a).block_id).to_be(olda)
  -- Refresh: revision 2. The old revision is stale now.
  expect(form:action('refresh', 'refresh', {}).status).to_be('succeeded')
  tree = form:render()
  expect(node(tree, 'restart').action.name).to_be('restart@2')
  expect(node(tree, 'restart').action.enabled).to_be(true)
  local stale = form:action('restart', 'restart@1', {})
  expect(stale.status).to_be('failed')
  expect(code(stale)).to_contain('stale')
  expect(form:action('restart', 'restart@2', {}).status).to_be('succeeded')
  tree = done(form)
  expect(item(tree, 'outcome', a).detail).to_contain('rebound')
  expect(item(tree, 'outcome', b).detail).to_contain('rebound')
  form:action('close', 'close', {})
end)

test('an agent pane cannot open the form', function()
  local _, _, sess = started('f5')
  expect_error(function()
    alice:hub('hub.admin.restart.form', {kind = 'codex', ctx = {client_id = H.self_id, session_id = sess}})
  end)
  expect(#H.views(owner)).to_be(0)
end)

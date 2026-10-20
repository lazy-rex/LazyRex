-- Agent identity, stage 2 on the hub (hub/cmd/hub/identity.go): a pane that never joins is known by
-- its report alone; hub.agents lists identities; hub.agents.resolve by ID, prefix, tab name and
-- legacy alias; a joined legacy pane's first report makes a permanent alias. It replaces the Node
-- lab identity-stage2-lab.mjs. The notice part of that lab belongs to Messaging.

local function agents(all)
  local list = H.hub('hub.agents', {all = all or nil}).agents or {}
  local out = {}
  for _, a in ipairs(list) do out[a.id] = a end
  return out
end

local function resolve(to) return H.hub('hub.agents.resolve', {to = to}).id end

-- planner is a pane that never joins, with the identity claude:plan-2041. A repeat report is unchanged.
local function planner()
  local p = agent('planner', {join = false, title = 'Planner'})
  return p, p:report({kind = 'claude', conversation_id = 'plan-2041'})
end

test('a pane that never joins is known by its report alone', function()
  local p, r = planner()
  expect(r.state).to_be('current')
  expect(r.agent_id, 'no legacy agent in this pane').to_be(nil)
  expect(r.name).to_be('Planner')
  local e = agents()['claude:plan-2041']
  expect(e.name).to_be('Planner')
  expect(e.state).to_be('current')
  expect(e.joined).to_be(true)
  expect(#(e.aliases or {})).to_be(0)
  expect(p:call('fix.who', {}).agent, 'the plugin identifies the pane through the hub').to_be('claude:plan-2041')
end)

test('resolve by ID, unique tab name and unique prefix; unknown and short prefixes fail', function()
  planner()
  expect(resolve('claude:plan-2041')).to_be('claude:plan-2041')
  expect(resolve('planner'), 'a unique tab name').to_be('claude:plan-2041')
  expect(resolve('claude:plan-204'), 'a unique prefix').to_be('claude:plan-2041')
  expect_error(function() resolve('nobody-at-all') end, 'unknown')
  expect_error(function() resolve('claude:pl') end, 'unknown') -- a prefix needs 4 characters after the colon
end)

test('a pane that never joins can spawn: the spawn records the identity as its caller', function()
  local p = planner()
  local s = p:hub('hub.spawn', {profile = 'shell', key = 'id-spawn-1'})
  expect(s.status).to_be('open')
  local parent
  for _, w in ipairs(p:hub('hub.workers', {}).workers) do
    if w.spawn_id == s.spawn_id then parent = w.parent end
  end
  expect(parent).to_be('claude:plan-2041')
  expect(p:hub('hub.stop', {spawn_id = s.spawn_id}).status).to_be('closed')
end)

test('a joined legacy pane\'s first report makes a permanent alias', function()
  local d = agent('dave')
  local r = d:report({kind = 'claude', conversation_id = 'dave-conv-1'})
  expect(r.agent_id).to_be('dave')
  local list = agents()
  expect(list['dave'], 'an aliased legacy agent leaves the default list').to_be(nil)
  expect(list['claude:dave-conv-1'].aliases).to_equal({'dave'})
  expect(agents(true)['dave'].alias_of).to_be('claude:dave-conv-1')
  expect(resolve('dave'), 'a legacy ID resolves to its identity').to_be('claude:dave-conv-1')

  r = d:report({kind = 'claude', conversation_id = 'dave-conv-2', source = 'clear'})
  expect(r.predecessor).to_be('claude:dave-conv-1')
  expect(agents()['claude:dave-conv-1'], 'a superseded identity leaves the default list').to_be(nil)
  expect(agents(true)['claude:dave-conv-1'].state).to_be('superseded')
  expect(resolve('dave'), 'the alias is permanent: it follows the conversation').to_be('claude:dave-conv-1')
end)

test('the agent face lists agent actions and plugin exports, not internal actions', function()
  local face
  for _, c in ipairs(rex.call('client.list', {}).clients or {}) do
    if c.info and c.info.label == 'lazyrex' and c.connection_state == 'connected' then face = c.client_id end
  end
  expect(face, 'the agent face client').to_be_truthy()
  local names = {}
  local actions = rex.call('client.inspect', {client_id = face}).actions or {}
  for _, a in ipairs(actions) do names[a.name] = true end
  for _, n in ipairs({'hub.spawn', 'hub.agents', 'fix.put'}) do expect(names[n], 'lists ' .. n).to_be(true) end
  for _, n in ipairs({'hub.admin.stop', 'hub.identity.report', 'hub.register'}) do expect(names[n], 'not ' .. n).to_be(nil) end
  local list = planner():call('hub.agents', {}).agents or {}
  local found = false
  for _, a in ipairs(list) do found = found or a.id == 'claude:plan-2041' end
  expect(found, 'hub.agents through the agent face').to_be(true)
end)

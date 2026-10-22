-- Test agents in identity mode (lab/test/harness.lua, H.agent): each agent reports
-- "<kind>:<name>" from its own pane, and the harness waits until the hub has it current there.

local function ident(id)
  for _, x in ipairs(H.hub('hub.identity.list', {}).identities or {}) do
    if x.identity == id then return x end
  end
  return nil
end

-- current_in returns the identity that is current in the pane, or nil.
local function current_in(block)
  for _, x in ipairs(H.hub('hub.identity.list', {}).identities or {}) do
    if x.state == 'current' and x.block_id == block then return x.identity end
  end
  return nil
end

test('an agent is current in its own pane, for each kind', function()
  local a = agent('ida')
  expect(a.id).to_be('claude:ida')
  expect(a.name).to_be('ida')
  expect(a.kind).to_be('claude')
  expect(ident('claude:ida').block_id).to_be(a.block)
  local c = agent('idc', {kind = 'codex'})
  expect(c.id).to_be('codex:idc')
  expect(ident('codex:idc').block_id).to_be(c.block)
  expect(c.block).not_to_be(a.block)
end)

test('report = false gives no identity until a report', function()
  local a = agent('quiet', {report = false})
  expect(a.id).to_be(nil)
  expect(ident('claude:quiet')).to_be(nil)
  expect(current_in(a.block)).to_be(nil)
  a:report({kind = 'claude', conversation_id = 'quiet'})
  expect(a.id).to_be('claude:quiet')
  expect(ident('claude:quiet').block_id).to_be(a.block)
end)

test('join is refused in identity mode', function()
  expect_error(function() agent('joiner', {join = 'joiner'}) end, 'join is the roster mode')
  expect_error(function() agent('joiner', {join = false}) end, 'join is the roster mode')
end)

test('a bad kind and the name user are refused', function()
  expect_error(function() agent('kinded', {kind = 'gemini'}) end, 'kind is claude or codex')
  expect_error(function() agent('user') end, 'UI user')
end)

test('a second call returns the same agent; other options are an error', function()
  local a = agent('twice')
  expect(agent('twice')).to_be(a)
  expect(agent('twice', {kind = 'claude'})).to_be(a)
  expect_error(function() agent('twice', {kind = 'codex'}) end, 'already open')
  expect_error(function() agent('twice', {report = false}) end, 'already open')
end)

test('exit, then the same name opens a new pane', function()
  local a = agent('again')
  local first = a.block
  a:exit()
  local b = agent('again')
  expect(b).not_to_be(a)
  expect(b.block).not_to_be(first)
  expect(ident('claude:again').block_id).to_be(b.block)
end)

test('a failed report fails the call and caches nothing', function()
  -- '!' is not allowed in a conversation_id (hub/cmd/hub/identity.go).
  expect_error(function() agent('bad!') end, 'conversation_id must be')
  expect(ident('claude:bad!')).to_be(nil)
  expect_error(function() agent('bad!') end, 'conversation_id must be')
end)

test('an identity current in another pane moves to the new pane', function()
  local holder = agent('holder', {report = false})
  holder:report({kind = 'claude', conversation_id = 'shared'})
  expect(ident('claude:shared').block_id).to_be(holder.block)
  local s = agent('shared')
  expect(s.block).not_to_be(holder.block)
  local x = ident('claude:shared')
  expect(x.state, 'a move is not superseded').to_be('current')
  expect(x.block_id).to_be(s.block)
  expect(current_in(holder.block), 'the old pane has no current identity').to_be(nil)
end)

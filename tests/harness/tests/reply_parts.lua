-- Reply parts (docs/hub.md "Reply parts"). The fixture route /lazy/parts-<x> renders about kb KiB
-- (the SQL row parts_spec(<x>)) and sends the reply with hub.reply.part, as the SDK does; mode
-- 'gap' skips a part, 'bytes' sends a wrong byte count, 'inline' sends one message.

local alice = agent('alice')

local function sql(text, ...)
  local r = alice:call('fix.hub', {action = 'hub.sql.exec', args = {sql = text, params = {...}}})
  expect(r.error, text).to_be(nil)
end

local function count(v)
  for _, c in ipairs(renders()) do
    if c.view_id == v.id then return c end
  end
  return {attempts = 0, causes = {}}
end

local function total()
  local n = 0
  for _, c in ipairs(renders()) do n = n + c.attempts end
  return n
end

-- settle waits until no render starts for one second.
local function settle()
  local last = -1
  wait('the renders to settle', function()
    local n = total()
    if n == last then return true end
    last = n
    rex.sleep(1)
    return nil
  end, 20)
end

sql('CREATE TABLE parts_spec (name TEXT PRIMARY KEY, kb INTEGER, mode TEXT, refresh TEXT)')
sql('CREATE TABLE other (x)')

local function open(name, kb, mode, refresh)
  sql('CREATE TABLE parts_' .. name .. ' (x)')
  sql('INSERT INTO parts_spec VALUES (?, ?, ?, ?)', name, kb, mode or 'ok', refresh or {null = true})
  return user:open('lazy/parts-' .. name)
end

-- The sha256 of each filler text (fixture.lua filler) and of the 200 KiB result of fix.big.
local FILLER = 'f73d41167a57f88a0bea7e7b5220e87fdbd7f1b65bb0df63a9c0fc9fb5ef1b81'
local BIG = '5918d8e4881ed30f06ce8465dbccc340c68eb8ffa30e02d81bcc9db024076db0'

local function whole(tree, what)
  expect(node(tree, 'last').text, what .. ': the last node').to_be('end')
  expect(node(tree, 'n1') ~= nil, what .. ': the first node').to_be(true)
end

-- whole_short checks every node of a short snapshot (v:get_short): n1..nN in order, each with
-- the filler text, then count and last.
local function whole_short(tree, what)
  local nodes = tree.nodes
  for k = 1, #nodes - 2 do
    expect(nodes[k].id, what .. ': node ' .. k).to_be('n' .. k)
    expect(nodes[k].text.sha256, what .. ': the text of node ' .. k).to_be(FILLER)
    expect(nodes[k].text.len, what .. ': the length of node ' .. k).to_be(600)
  end
  expect(nodes[#nodes].id, what .. ': the last node').to_be('last')
  return #nodes - 2
end

test('a 150 KiB view comes whole through parts to the renderer', function()
  local v = open('a', 150)
  local tree = v:render()
  whole(tree, 'render')
  expect(node(tree, 'count').text).to_be('0')
  local n = whole_short(v:get_short().value.tree, 'hub.view.get (the renderer snapshot)')
  expect(n > 100, 'filler nodes').to_be(true)
  sql('INSERT INTO parts_a VALUES (1)')
  wait('the view to show the new row', function() return node(v:get_short().value.tree, 'count').text == '1' or nil end, 20)
end)

test('an ASCII reply of 50000 bytes, over inline_max, goes in one part and resolves', function()
  local _, base = calls('none')
  local v = open('b', 0, 'ascii')
  local tree = v:render()
  expect(node(tree, 'last').text).to_be('end')
  -- Other open views render again at each commit and send their own parts: find this reply's
  -- part by its size.
  local one
  for _, c in ipairs(calls('hub.reply.part', {after = base.next})) do
    if #c.args.data == 50000 then one = c end
  end
  expect(one ~= nil, 'a part of 50000 bytes').to_be(true)
  expect(one.args.count, 'count').to_be(1)
  expect(one.args.index, 'index').to_be(0)
end)

test('a missing part, a wrong byte count, and a reply over the cap show the Error tree', function()
  for name, spec in pairs({c = {150, 'gap'}, d = {150, 'bytes'}, e = {800, 'ok'}}) do
    local v = open(name, spec[1], spec[2])
    local r = v:try_render()
    expect(r.status, name .. ': the render').to_be('failed')
    expect(node(v:get().tree, '$error') ~= nil, name .. ': the Error tree').to_be(true)
  end
end)

test('an opted-in view with a chunked tree stays tracked', function()
  local f = open('f', 150, 'ok', 'reads')
  settle()
  expect(count(f).sql_tracked, 'F tracked').to_be(true)
  local f0 = count(f).attempts
  sql('INSERT INTO other VALUES (1)')
  settle()
  expect(count(f).attempts, 'F after an unrelated write').to_be(f0)
  sql('INSERT INTO parts_f VALUES (1)')
  settle()
  expect(count(f).attempts, 'F after a write to its table').to_be(f0 + 1)
end)

test('an agent gets a 200 KiB result of a forwarded call whole', function()
  local r = alice:call_short('fix.big', {kb = 200})
  expect(r.value.kb).to_be(200)
  expect(r.value.data.len).to_be(200 * 1024)
  expect(r.value.data.sha256, 'the data').to_be(BIG)
  expect(r.bytes > 200 * 1024).to_be(true)
end)

test('measure: a 150 KiB view in parts and a 40 KiB view in one message', function()
  local big, small = open('g', 150), open('h', 40, 'inline')
  local times = {big = {}, small = {}}
  for _ = 1, 5 do
    big:render()
    small:render()
    times.big[#times.big + 1] = count(big).last_call_ms
    times.small[#times.small + 1] = count(small).last_call_ms
  end
  table.sort(times.big)
  table.sort(times.small)
  print('view.render call ms (median of 5): 150 KiB in parts ' .. times.big[3] .. ', 40 KiB inline ' .. times.small[3])
  expect(times.big[3] - times.small[3] < 50, 'parts add less than 50 ms').to_be(true)
end)

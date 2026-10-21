-- Declared render dependencies (docs/hub.md, "Render dependencies"). The fixture route
-- /lazy/deps-<x> declares the record spec/deps-<x> as its deps and shows data/deps-<x>.

local alice = agent('alice')

local function put(key, value) return alice:call('fix.put', {key = key, value = value}) end
local function del(key) return alice:call('fix.hub', {action = 'hub.store.batch', args = {ops = {{key = key, delete = true}}}}) end

-- count returns the render counts of a view (zero counts before its first render).
local function count(v)
  for _, c in ipairs(renders()) do
    if c.view_id == v.id then return c end
  end
  return {attempts = 0, ok = 0, failed = 0, transient = 0, causes = {}}
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

local function open(name, spec)
  if spec ~= nil then put('spec/deps-' .. name, spec) end
  local v = user:open('lazy/deps-' .. name)
  v:render()
  return v
end

test('a commit renders only the views whose prefixes match it, also for a delete', function()
  local a = open('a', {prefixes = {'data/deps-a', 'spec/deps-a'}})
  local b = open('b', {prefixes = {'data/deps-b', 'spec/deps-b'}})
  settle()
  local a0, b0 = count(a).attempts, count(b).attempts
  put('data/deps-a', 'one')
  settle()
  expect(count(a).attempts, 'A after its write').to_be(a0 + 1)
  expect(count(b).attempts, 'B after A write').to_be(b0)
  expect(count(a).causes.commit > 0).to_be(true)
  del('data/deps-a')
  settle()
  expect(count(a).attempts, 'A after its delete').to_be(a0 + 2)
  expect(count(b).attempts, 'B after A delete').to_be(b0)
  put('other/x', 1)
  settle()
  expect(count(a).attempts, 'A after an unrelated write').to_be(a0 + 2)
  expect(count(b).attempts, 'B after an unrelated write').to_be(b0)
end)

test('no deps renders on each commit; an explicit empty list on none', function()
  local c = open('c', nil)
  local d = open('d', {prefixes = {}})
  settle()
  local c0, d0 = count(c).attempts, count(d).attempts
  put('other/y', 1)
  settle()
  expect(count(c).attempts, 'no deps').to_be(c0 + 1)
  expect(count(d).attempts, 'explicit empty').to_be(d0)
end)

test('malformed deps fall back to a render on each commit, with one view_deps_invalid', function()
  local e = open('e', {prefixes = 'not a list'})
  settle()
  local e0 = count(e).attempts
  put('other/z', 1)
  settle()
  put('other/z', 2)
  settle()
  expect(count(e).attempts).to_be(e0 + 2)
  local n = 0
  for _, ev in ipairs(H.hub('hub.test.events', {event = 'view_deps_invalid'}).events or {}) do
    if ev.route == '/lazy/deps-e' then n = n + 1 end
  end
  expect(n, 'one event for one error text').to_be(1)
end)

test('a commit or a route invalidate during a held render renders the view again', function()
  local f = open('f', {prefixes = {'data/deps-f'}})
  local g = open('g', {prefixes = {'data/deps-g'}})
  settle()
  local f0, g0 = count(f).attempts, count(g).attempts
  local id = fault({point = 'view-render', mode = 'hold', key = '/lazy/deps-f'})
  put('data/deps-f', 'held')
  wait('the hold', function() return fault_fired(id) end, 10)
  -- Neither the old nor the new prefixes match this key: the render is in flight, so it counts.
  put('spec/deps-f', {prefixes = {'data/deps-f2'}})
  alice:call('fix.hub', {action = 'hub.ui.invalidate', args = {routes = {'/lazy/deps-f'}}})
  fault_release(id)
  settle()
  expect(count(f).attempts, 'the held render and one more pass').to_be(f0 + 2)
  expect(count(g).attempts, 'the other route').to_be(g0)
  expect(count(f).causes.invalidate > 0).to_be(true)
  -- The new declaration is installed: data/deps-f no longer renders f.
  put('data/deps-f', 'after')
  settle()
  expect(count(f).attempts).to_be(f0 + 2)
end)

test('opening a view renders only that view', function()
  local h = open('h', {prefixes = {}})
  settle()
  local h0 = count(h).attempts
  local i = open('i', {prefixes = {}})
  settle()
  expect(count(h).attempts, 'the other view').to_be(h0)
  expect(count(i).attempts > 0).to_be(true)
end)

test('hub.ui.invalidate checks its routes', function()
  local r = alice:call('fix.hub', {action = 'hub.ui.invalidate', args = {routes = {}}})
  expect(r.error ~= nil and r.error.message:find('routes') ~= nil, show(r)).to_be(true)
  r = alice:call('fix.hub', {action = 'hub.ui.invalidate', args = {routes = {''}}})
  expect(r.error ~= nil, show(r)).to_be(true)
end)

test('an operation that leaves the tree the same renders only its own view and notifies it', function()
  local j = open('j', {prefixes = {}})
  local k = open('k', {prefixes = {}})
  settle()
  local j0, k0, n0 = count(j).attempts, count(k).attempts, j:status().changes
  local op = j:action('lazy', 'poke', {}, 'deps-poke-1')
  expect(op.status, show(op)).to_be('succeeded')
  settle()
  expect(count(j).attempts, 'the view of the operation').to_be(j0 + 1)
  expect(count(j).causes.operation > 0).to_be(true)
  expect(count(k).attempts, 'the other view').to_be(k0)
  wait('view.changed for the operation', function() return j:status().changes > n0 end, 10)
end)

test('a registration during a held render: the old reply installs nothing', function()
  local r = open('r', {prefixes = {'data/deps-r-old'}})
  settle()
  local first = fault({point = 'view-render', mode = 'hold', key = '/lazy/deps-r'})
  local second = fault({point = 'view-render', mode = 'hold', key = '/lazy/deps-r'})
  alice:call('fix.hub', {action = 'hub.ui.invalidate', args = {routes = {'/lazy/deps-r'}}})
  wait('the first hold', function() return fault_fired(first) end, 10)
  -- The held reply declares data/deps-r-old. The plugin then declares data/deps-r-new and registers again.
  put('spec/deps-r', {prefixes = {'data/deps-r-new'}})
  alice:call('fix.register', {})
  fault_release(first)
  wait('the second hold', function() return fault_fired(second) end, 10)
  expect(count(r).declared, 'after the old reply, before the new one').to_be(nil)
  fault_release(second)
  settle()
  expect(show(count(r).declared and count(r).declared.prefixes)).to_be(show({'data/deps-r-new'}))
  local r0 = count(r).attempts
  put('data/deps-r-old', 1)
  settle()
  expect(count(r).attempts, 'the old prefix').to_be(r0)
  put('data/deps-r-new', 1)
  settle()
  expect(count(r).attempts, 'the new prefix').to_be(r0 + 1)
end)

test('a close during a held render: the reply does not make the view state again', function()
  local s = open('s', {prefixes = {'data/deps-s'}})
  settle()
  local id = fault({point = 'view-render', mode = 'hold', key = '/lazy/deps-s'})
  put('data/deps-s', 'held')
  wait('the hold', function() return fault_fired(id) end, 10)
  H.perform(s.client, 'testview.close', {})
  wait('the view is closed', function()
    for _, w in ipairs(H.views()) do
      if w.view_id == s.id and w.status ~= 'closed' then return nil end
    end
    return true
  end, 10)
  fault_release(id)
  settle()
  expect(count(s).declared, 'no declaration for a closed view').to_be(nil)
  local s0 = count(s).attempts
  put('data/deps-s', 'after')
  settle()
  expect(count(s).attempts, 'no render of a closed view').to_be(s0)
end)

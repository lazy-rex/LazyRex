-- Declared render dependencies (docs/hub.md, "Render dependencies"). The fixture route
-- /lazy/deps-<x> declares the kv row spec/deps-<x> as its deps and shows data/deps-<x>. Its
-- queries carry the render ID, so the view tracks the table kv: every kv commit renders it again.
-- A declaration filters only changed().

local alice = agent('alice')

local function put(key, value) return alice:call('fix.put', {key = key, value = value}) end

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

test('malformed deps fall back to a render on each commit, with one view_deps_invalid', function()
  local e = open('e', {changes = 'not a list'})
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
  local f = open('f', {changes = {}})
  local g = open('g', {changes = {}})
  settle()
  local f0, g0 = count(f).attempts, count(g).attempts
  local id = fault({point = 'view-render', mode = 'hold', key = '/lazy/deps-f'})
  alice:call('fix.hub', {action = 'hub.ui.invalidate', args = {routes = {'/lazy/deps-f'}}})
  wait('the hold', function() return fault_fired(id) end, 10)
  -- The commit renders g once. The render of f is in flight, so the commit and the invalidate
  -- render f once more after the held render.
  put('data/deps-f', 'held')
  alice:call('fix.hub', {action = 'hub.ui.invalidate', args = {routes = {'/lazy/deps-f'}}})
  fault_release(id)
  settle()
  expect(count(f).attempts, 'the held render and one more pass').to_be(f0 + 2)
  expect(count(g).attempts, 'the other route, for the commit').to_be(g0 + 1)
  expect(count(f).causes.invalidate > 0).to_be(true)
end)

test('opening a view renders only that view', function()
  local h = open('h', nil)
  settle()
  local h0 = count(h).attempts
  local i = open('i', nil)
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
  local j = open('j', nil)
  local k = open('k', nil)
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
  local r = open('r', {changes = {'agents'}})
  settle()
  local first = fault({point = 'view-render', mode = 'hold', key = '/lazy/deps-r'})
  local second = fault({point = 'view-render', mode = 'hold', key = '/lazy/deps-r'})
  alice:call('fix.hub', {action = 'hub.ui.invalidate', args = {routes = {'/lazy/deps-r'}}})
  wait('the first hold', function() return fault_fired(first) end, 10)
  -- The held reply declares agents. The plugin then declares workers and registers again.
  put('spec/deps-r', {changes = {'workers'}})
  alice:call('fix.register', {})
  fault_release(first)
  wait('the second hold', function() return fault_fired(second) end, 10)
  expect(count(r).declared, 'after the old reply, before the new one').to_be(nil)
  fault_release(second)
  settle()
  expect(show(count(r).declared and count(r).declared.changes)).to_be(show({'workers'}))
end)

test('a close during a held render: the reply does not make the view state again', function()
  local s = open('s', {changes = {}})
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

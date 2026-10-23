-- SQL read tracking, the default for each render (docs/hub.md, "SQL read tracking"). The fixture route
-- /lazy/reads-<x> reads the row reads_spec(<x>) and counts the rows of reads_<x>; the row gives
-- the refresh value of the reply and the mode of the read (fixture.lua).

local alice = agent('alice')

local function sql(text, ...)
  local r = alice:call('fix.hub', {action = 'hub.sql.exec', args = {sql = text, params = {...}}})
  expect(r.error, text).to_be(nil)
end

-- count returns the render counts of a view (zero counts before its first render).
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

sql('CREATE TABLE reads_spec (name TEXT PRIMARY KEY, refresh TEXT, mode TEXT)')
sql('CREATE TABLE other (x)')

local function open(name, refresh, mode)
  sql('CREATE TABLE reads_' .. name .. ' (x)')
  sql('INSERT INTO reads_spec VALUES (?, ?, ?)', name, refresh or {null = true}, mode or 'tokened')
  local v = user:open('lazy/reads-' .. name)
  v:render()
  return v
end

test('a view without refresh renders again only after a write to a table that it read, or DDL', function()
  local a = open('a', nil)
  local b = open('b', nil)
  settle()
  expect(count(a).sql_tracked, 'A tracked').to_be(true)
  expect(count(b).sql_tracked, 'B tracked').to_be(true)
  local a0, b0 = count(a).attempts, count(b).attempts
  sql('INSERT INTO reads_a VALUES (1)')
  settle()
  expect(count(a).attempts, 'A after its write').to_be(a0 + 1)
  expect(count(b).attempts, 'B after the write to reads_a').to_be(b0)
  expect(node(a:render(), 'lazy').text, 'A shows the new row').to_be('1') -- v:render() renders A once more
  settle()
  a0 = count(a).attempts
  sql('INSERT INTO other VALUES (1)')
  settle()
  expect(count(a).attempts, 'A after an unrelated write').to_be(a0)
  expect(count(b).attempts, 'B after an unrelated write').to_be(b0)
  sql('CREATE TABLE ddl_marks_all (x)')
  settle()
  expect(count(a).attempts, 'A after DDL').to_be(a0 + 1)
  expect(count(b).attempts, 'B after DDL').to_be(b0 + 1)
end)

test('with all, or untracked: every SQL commit renders the view', function()
  local views = {
    all = open('c', 'all'),
    untokened = open('f', nil, 'untokened'),
    other = open('g', nil, 'other'),
    refused = open('i', nil, 'refused'),
  }
  settle()
  local before = {}
  for name, v in pairs(views) do
    expect(count(v).sql_tracked, name .. ' tracked').to_be(false)
    before[name] = count(v).attempts
  end
  sql('INSERT INTO other VALUES (2)')
  settle()
  for name, v in pairs(views) do
    expect(count(v).attempts, name .. ' after an unrelated write').to_be(before[name] + 1)
  end
end)

test('a render that sends all returns to broad refresh', function()
  local h = open('h', nil)
  settle()
  expect(count(h).sql_tracked, 'H tracked').to_be(true)
  sql("UPDATE reads_spec SET refresh = 'all' WHERE name = 'h'") -- reads_spec is read: H renders again
  settle()
  expect(count(h).sql_tracked, 'H after all').to_be(false)
  local h0 = count(h).attempts
  sql('INSERT INTO other VALUES (3)')
  settle()
  expect(count(h).attempts, 'H after an unrelated write').to_be(h0 + 1)
end)

test('any other refresh value is a declaration error: no table set, one view_deps_invalid', function()
  local k = open('k', 'reads')
  settle()
  expect(count(k).sql_tracked, 'K tracked').to_be(false)
  local k0 = count(k).attempts
  sql('INSERT INTO other VALUES (4)')
  settle()
  expect(count(k).attempts, 'K after an unrelated write').to_be(k0 + 1)
  local errors = {}
  for _, ev in ipairs(H.hub('hub.test.events', {event = 'view_deps_invalid'}).events or {}) do
    if ev.route == '/lazy/reads-k' then errors[#errors + 1] = ev.error end
  end
  expect(#errors, 'one event for one error text').to_be(1)
  expect(errors[1]).to_be('refresh must be absent or "all"; got "reads"')
end)

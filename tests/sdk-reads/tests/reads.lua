-- The real SDK of this checkout (sdk/lua) against the hub's SQL read tracking (codex-29's review lab,
-- Hub Render 4.2). Run: lazyrex-hub test tests/sdk-reads (from the repository root).
-- The SDK's own calls in a render keep it tracked: a default panel renders again only after a
-- write to the table that it read. A panel with refresh = "all" renders after every write.
local alice = agent('alice')
local function pane(name)
  local s = session(name)
  local w = rex.call('session.new_window', {session_id = s, window_label = 'work',
    layout = {block = {flavor = 'com.superlogical.terminal.shell', options = {command = {'/bin/sleep', '600'}, login = false}}}})
  return {session = s, block = w.block_ids[1]}
end
local function count(v)
  for _, c in ipairs(renders('rr')) do if c.view_id == v.id then return c end end
  return {attempts = 0}
end
local function total()
  local n = 0
  for _, c in ipairs(renders('rr')) do n = n + c.attempts end
  return n
end
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

test('default SDK panels stay tracked and render only after their own table changes', function()
  -- One pane in its own session for each panel: a second panel in the same window replaces the
  -- first (the panel slot).
  local views = {}
  for _, id in ipairs({'alpha', 'beta', 'gamma'}) do
    local p = pane('rr-' .. id)
    user:command(id, p)
    views[id] = user:view('/panels/' .. id, 'rr', p.session)
    views[id]:render()
  end
  settle()
  expect(count(views.alpha).sql_tracked, 'alpha tracked').to_be(true)
  expect(count(views.beta).sql_tracked, 'beta tracked').to_be(true)
  expect(count(views.gamma).sql_tracked, 'gamma (refresh = all) tracked').to_be(false)
  local a0, b0, g0 = count(views.alpha).attempts, count(views.beta).attempts, count(views.gamma).attempts
  alice:call('rr.add', {t = 'other'})
  settle()
  expect(count(views.alpha).attempts, 'alpha after a write to other').to_be(a0)
  expect(count(views.beta).attempts, 'beta after a write to other').to_be(b0)
  expect(count(views.gamma).attempts, 'gamma after a write to other').to_be(g0 + 1)
  expect(count(views.alpha).sql_tracked, 'alpha still tracked').to_be(true)
  alice:call('rr.add', {t = 'a'})
  settle()
  expect(count(views.alpha).attempts, 'alpha after a write to a').to_be(a0 + 1)
  expect(count(views.beta).attempts, 'beta after a write to a').to_be(b0)
  expect(count(views.gamma).attempts, 'gamma after a write to a').to_be(g0 + 2)
  wait('alpha shows the new row', function() return node((views.alpha:get() or {}).tree or {nodes = {}}, 'n') and node(views.alpha:get().tree, 'n').text == '1' end, 10)
end)

test('caught errors before hub dispatch give broad refresh in both SDK query paths', function()
  for _, path in ipairs({'db', 'raw'}) do
    local gate = fault({point = 'action', action = 'hub.sql.query', when = 'before', mode = 'gate',
      sql_param = 'review-' .. path .. '-fail', plugin = 'rr'})
    local p = pane('review-caught-' .. path)
    user:command('caught_' .. path, p)
    local v = user:view('/panels/caught_' .. path, 'rr', p.session)
    wait('the query gate', function() return fault_fired(gate) end, 10)
    settle()
    expect(node(v:get().tree, 'caught').text).to_be('caught')
    expect(count(v).sql_tracked, 'the SDK replies refresh = all after a caught error').to_be(false)
    local before = count(v).attempts
    alice:call('rr.add', {t = 'other'})
    settle()
    expect(count(v).attempts, 'broad refresh after the caught query error').to_be(before + 1)
    expect(count(v).sql_tracked).to_be(false)
    fault_release(gate)
    v:render()
    settle()
    expect(node(v:get().tree, 'caught').text).to_be('complete')
    expect(count(v).sql_tracked, 'a later complete render is tracked again').to_be(true)
  end
end)

test('hub.workers and hub.profiles in a render pass the declared schemas and keep the render tracked', function()
  local p = pane('rr-hub-reads')
  user:command('hub_reads', p)
  local v = user:view('/panels/hub_reads', 'rr', p.session)
  v:render()
  settle()
  wait('the panel shows its reads', function() local t = (v:get() or {}).tree; return t and node(t, 'reads') and node(t, 'reads').text == 'ok' end, 10)
  expect(count(v).sql_tracked, 'a render with covered reads only is tracked').to_be(true)
  local before = count(v).attempts
  alice:call('rr.add', {t = 'a'})
  settle()
  expect(count(v).attempts, 'a table commit does not mark it').to_be(before)
end)

-- The plugin manager views (C8): Plugins… lists the entries, an entry menu sends its ops as the
-- plugins view effect, and a form that sent an op stays open and shows that op. The managed
-- entries are tests/stubs/managed (plugin mstub; arg crash=1 makes it crash) and
-- tests/stubs/dependent (plugin mdep, which depends on mstub), written into the lab's managed file
-- and loaded with hub.plugins.reload. Agent path: hub.plugins.op on the agent face.

local codex = agent('codex', {kind = 'codex', title = 'Codex'})
local checkout = H.args.sdk:gsub('/sdk/lua$', '')
local stub = checkout .. '/plugins/lazyrex/tests/stubs/managed'
local dependent = checkout .. '/plugins/lazyrex/tests/stubs/dependent'
local KEY = 'managed:ms'

local function hex(s) return (s:gsub('.', function(c) return string.format('%02x', c:byte()) end)) end
-- managed writes the managed file: ms with args, and md when with_dependent, then reloads.
local function managed(args, with_dependent)
  local list = {{id = 'ms', source = stub, args = args}}
  if with_dependent then list[2] = {id = 'md', source = dependent} end
  local f = assert(io.open(H.args.lab .. '/hub/plugins.json', 'w'))
  f:write(rex.json.encode({plugins = list}))
  f:close()
  H.hub('hub.plugins.reload', {})
end
local function entry(id)
  for _, e in ipairs(H.hub('hub.plugins', {}).entries or {}) do if e.managed == (id or 'ms') then return e end end
end
local function running() local e = entry(); return e and e.state == 'running' end
local function menu(p, key)
  key = key or KEY
  expect(p:action('entries', 'manage', {item_id = key}).status).to_be('succeeded')
  local m = user:view('/forms/plugins/entry/' .. hex(key), nil, p.session)
  return m, m:render()
end
local function settings_node(tree)
  for _, n in ipairs(tree.nodes) do if n.id:sub(1, 9) == 'settings/' and n.type == 'form' then return n end end
end
local function progress(v, want)
  local text
  wait('the progress shows ' .. want, function()
    text = node(v:render(), 'progress').text
    return text:find(want, 1, true) ~= nil
  end)
  return text
end
local function open_view(id)
  for _, w in ipairs(views()) do if w.view_id == id then return w end end
end

managed({k = 'v'})
wait('mstub runs', running)

test('Plugins… lists the managed entry; its menu shows the settings and no operation yet', function()
  local p = user:panel('plugins')
  local tree = p:render()
  local item
  for _, i in ipairs(node(tree, 'entries').items) do if i.id == KEY then item = i end end
  expect(item.label).to_be('mstub')
  expect(item.detail:find('running', 1, true) ~= nil).to_be(true)
  expect(item.detail:find(stub, 1, true) ~= nil).to_be(true) -- the source of the managed entry
  local _, mt = menu(p)
  expect(node(mt, 'progress').text).to_be('No operation yet.')
  local s = settings_node(mt)
  expect(s.fields[1].value).to_be('k=v')
  expect(s.fields[2].value).to_be('')
  expect(node(mt, 'update')).to_be(nil) -- a local entry has no update
end)

test('Save settings: the form stays open, shows done, and the entry has the new args', function()
  local m, mt = menu(user:panel('plugins'))
  local s = settings_node(mt)
  local op = m:action(s.id, 'settings', {values = {args = 'k=w\nempty=', watch_roots = ''}}, 'set-1')
  expect(op.status).to_be('succeeded')
  progress(m, 'settings · done')
  expect(open_view(m.id) ~= nil).to_be(true)
  expect(entry().args).to_equal({k = 'w', empty = ''})
  -- The same values again: the hub ends the op at admission with "no change".
  local s2 = settings_node(m:render())
  expect(m:action(s2.id, 'settings', {values = {args = 'empty=\nk=w', watch_roots = ''}}, 'set-2').status).to_be('succeeded')
  progress(m, 'no change')
end)

test('a settings form whose values changed since the render gets conflict and sends no op', function()
  local m, mt = menu(user:panel('plugins'))
  local old = settings_node(mt)
  local cur = entry()
  local done = codex:call('hub.plugins.op', {key = 'agent-set', op = 'settings', entry = KEY, input = {args = {k = 'agent'}}})
  wait('the agent op ends', function() return codex:call('hub.plugins.ops', {op_id = done.op_id}).phase == 'done' end)
  expect(cur.args.k ~= 'agent').to_be(true)
  local op = m:action(old.id, 'settings', {values = {args = 'k=mine', watch_roots = ''}}, 'set-stale')
  expect(op.status).to_be('failed')
  expect(op.error.code).to_be('conflict')
  expect(entry().args).to_equal({k = 'agent'})
  -- The agent's op is not this view's op.
  expect(node(m:render(), 'progress').text).to_be('No operation yet.')
end)

test('a value with a line break: the menu has no settings form', function()
  local done = codex:call('hub.plugins.op', {key = 'agent-nl', op = 'settings', entry = KEY, input = {args = {k = 'a\nb'}}})
  wait('the agent op ends', function() return codex:call('hub.plugins.ops', {op_id = done.op_id}).phase == 'done' end)
  local _, mt = menu(user:panel('plugins'))
  expect(settings_node(mt)).to_be(nil)
  expect(node(mt, 'settings').text:find('cannot be edited here', 1, true) ~= nil).to_be(true)
  managed({k = 'v'})
end)

-- The candidate-B refusal of an agent restart is TestPluginOpRestartAdmission (Go).
test('an agent restarts the entry with hub.plugins.op: a new mstub instance registers', function()
  wait('mstub runs', running)
  local function instances()
    local set = {}
    for _, e in ipairs(H.hub('hub.test.events', {event = 'plugin_registered'}).events or {}) do
      if e.plugin == 'mstub' then set[e.instance_id] = true end
    end
    return set
  end
  local before = instances()
  local op = codex:call('hub.plugins.op', {key = 'agent-restart', op = 'restart', entry = KEY})
  local ops
  wait('the agent restart ends', function()
    ops = codex:call('hub.plugins.ops', {op_id = op.op_id})
    return ops.phase == 'done'
  end)
  expect(ops.error or '').to_be('')
  wait('a new mstub instance registers', function()
    for id in pairs(instances()) do if not before[id] then return true end end
  end)
  wait('mstub runs', running)
end)

test('Disable keeps the entry stopped over a hub restart; Enable starts it again', function()
  local m = menu(user:panel('plugins'))
  expect(m:action('disable', 'disable', {}, 'dis-1').status).to_be('succeeded')
  progress(m, 'stop · done')
  wait('mstub stops', function() return entry().state == 'stopped' end)
  expect(entry().desired).to_be('stopped')
  hub:restart()
  expect(entry().state).to_be('stopped')
  local p = user:panel('plugins')
  local item
  for _, i in ipairs(node(p:render(), 'entries').items) do if i.id == KEY then item = i end end
  expect(item.detail:find('Disabled', 1, true) ~= nil).to_be(true)
  local m2 = menu(p)
  expect(m2:action('enable', 'enable', {}, 'en-1').status).to_be('succeeded')
  progress(m2, 'start · done')
  wait('mstub runs', running)
end)

test('Log opens the log panel with the pane output', function()
  local p = user:panel('plugins')
  local m = menu(p)
  expect(m:action('log', 'log', {}, 'log-1').status).to_be('succeeded')
  local l = user:view('/panels/plugins/log/' .. hex(KEY), nil, p.session)
  wait('the log shows the pane output', function()
    local pane = node(l:render(), 'pane')
    return pane and pane.text:find('mstub runs with k=', 1, true) ~= nil
  end)
end)

test('Remove… asks, then removes the entry; the confirm form stays open with the op', function()
  local p = user:panel('plugins')
  local m = menu(p)
  expect(m:action('remove', 'remove', {}, 'rm-open').status).to_be('succeeded')
  local c = user:view('/forms/plugins/confirm/remove/' .. hex(KEY), nil, p.session)
  c:render()
  expect(c:action('remove', 'remove', {}, 'rm-1').status).to_be('succeeded')
  progress(c, 'remove · done')
  expect(entry()).to_be(nil)
end)

test('Add plugin…: Next refuses a source with @ref and opens the version step for a GitHub source', function()
  local p = user:panel('plugins')
  p:render()
  expect(p:action('add', 'add', {}, 'add-open').status).to_be('succeeded')
  local a = user:view('/forms/plugins/add', nil, p.session)
  a:render()
  local bad = a:action('source', 'next', {values = {source = 'github:lab/r@v1'}}, 'add-bad')
  expect(bad.status).to_be('failed')
  expect(bad.error.code).to_be('invalid_input')
  expect(a:action('source', 'next', {values = {source = 'github:lab/r'}}, 'add-next').status).to_be('succeeded')
  local s = user:view('/forms/plugins/add/' .. hex('github:lab/r'), nil, p.session)
  local tree = s:render()
  expect(node(tree, 'shown_source').text).to_be('github:lab/r')
  expect(node(tree, 'add').fields[1].options[1].value).to_be('latest')
end)

-- The remove left mstub's record desired stopped; an added entry of the same name keeps it.
test('a crashing entry (desired running) has Disable and no Enable; Disable stops the retries', function()
  managed({crash = '1'})
  wait('ms is listed, stopped', function() local e = entry(); return e and e.state == 'stopped' end)
  local p = user:panel('plugins')
  local m = menu(p)
  expect(m:action('enable', 'enable', {}, 'crash-en').status).to_be('succeeded')
  wait('ms crashes', function() return entry().state == 'crashing' end)
  local mt = m:render()
  expect(node(mt, 'enable')).to_be(nil)
  expect(node(mt, 'disable') ~= nil).to_be(true)
  expect(m:action('disable', 'disable', {}, 'crash-dis').status).to_be('succeeded')
  progress(m, 'stop · done')
  wait('ms stops', function() local e = entry(); return e.state == 'stopped' and e.desired == 'stopped' end)
  expect(node(m:render(), 'disable')).to_be(nil)
end)

test('a waiting entry (its dependency is stopped) has Disable; Disable stops it', function()
  expect(entry().desired).to_be('stopped') -- the test above disabled ms
  managed({k = 'v'}, true)
  wait('md waits for mstub', function() local e = entry('md'); return e and e.state == 'waiting' end)
  local p = user:panel('plugins')
  local m, mt = menu(p, 'managed:md')
  expect(node(mt, 'enable')).to_be(nil)
  expect(node(mt, 'disable') ~= nil).to_be(true)
  expect(m:action('disable', 'disable', {}, 'wait-dis').status).to_be('succeeded')
  progress(m, 'stop · done')
  wait('md stops', function() local e = entry('md'); return e.state == 'stopped' and e.desired == 'stopped' end)
end)

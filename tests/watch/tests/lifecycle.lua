-- hub.watch across hub restarts, plugin restarts and directory changes (docs/hub.md, "File
-- watches": "Restart reconciliation" limits, "Initialization", "Plugin lifecycle").

local root = H.args.lab .. '/watched'

local function wf(action, args)
  local st = H.hub('hub.test.status', {})
  return H.perform(st.plugins.watchfix.client_id, action, args or {})
end

local function call(action, args)
  local r = wf('wf.hub', {action = action, args = args})
  if r.error then error(r.error.message, 0) end -- the text keeps the code (status=... code=...)
  return r.result
end

-- fresh removes every watch of the plugin: a plugin has at most 8.
local function fresh()
  local list = call('hub.watch.list', {}).watches
  for _, x in ipairs(list) do call('hub.watch.remove', {key = x.key, watch_id = x.watch_id}) end
end

local function sh(cmd)
  local r, err = rex.exec({'sh', '-c', cmd})
  assert(r and r.ok, 'command failed: ' .. cmd .. ': ' .. tostring(err or r.stderr))
end

local function put(path, text)
  local f = assert(io.open(path, 'w'))
  f:write(text)
  f:close()
end

local function dir(name)
  local d = root .. '/' .. name
  sh('mkdir -p ' .. d)
  return d
end

local function ensure(key, path, extra)
  local a = {key = key, path = path, settle_ms = 200}
  for k, v in pairs(extra or {}) do a[k] = v end
  return call('hub.watch.ensure', a)
end

local function read(w) return call('hub.watch.read', {key = w.key, watch_id = w.watch_id}) end
local function ack(w, through) return call('hub.watch.ack', {key = w.key, watch_id = w.watch_id, through = through}) end

local function state(w)
  local list = call('hub.watch.list', {}).watches
  for _, x in ipairs(list) do
    if x.watch_id == w.watch_id then return x.state end
  end
end

local function kinds(rows)
  local out = {}
  for _, r in ipairs(rows) do out[#out + 1] = r.name .. ':' .. r.kind end
  return table.concat(out, ' ')
end

local function wait_kinds(w, want, timeout)
  return wait('rows "' .. want .. '"', function()
    local p = read(w)
    if kinds(p.rows) == want then return p end
  end, timeout)
end

local function wait_state(w, want, timeout)
  return wait('state ' .. want, function() return state(w) == want end, timeout)
end

test('a hub restart keeps the watch and scans: a file that came and went leaves nothing', function()
  fresh()
  local d = dir('restart')
  local w = ensure('restart', d, {settle_ms = 3000})
  wait_state(w, 'active')
  -- Both changes are still settling when the hub stops, so nothing was committed.
  put(d .. '/kept.md', 'x')
  put(d .. '/brief.md', 'x')
  os.remove(d .. '/brief.md')
  hub:restart({crash = true})
  expect(state(w)).to_be_truthy()
  local p = wait_kinds(w, 'kept.md:created', 15)
  expect(p.rows[1].appearance_key).to_be(w.watch_id .. ':' .. p.rows[1].seq)
end)

test('a crash during initialization resumes it with the same baseline', function()
  fresh()
  local d = dir('initcrash')
  put(d .. '/base.md', 'start\n')
  sh('(i=0; while [ $i -lt 20 ]; do echo more >> ' .. d .. '/base.md; sleep 0.2; i=$((i+1)); done) >/dev/null 2>&1 &')
  local w = ensure('initcrash', d, {settle_ms = 500})
  expect(w.state).to_be('initializing')
  put(d .. '/later.md', 'x') -- not in the ensure listing: a later arrival
  hub:restart({crash = true})
  wait_state(w, 'active', 20)
  local p = wait_kinds(w, 'later.md:created')
  ack(w, p.next)
  -- base.md stays hidden until a change after initialization.
  put(d .. '/base.md', 'rewritten')
  wait_kinds(w, 'base.md:changed')
end)

test('a plugin restart keeps the watch, the rows wait, and the new instance gets a nudge', function()
  fresh()
  local d = dir('plugin')
  local w = ensure('plugin', d)
  wait_state(w, 'active')
  H.hub('hub.admin.stop', {plugin = 'watchfix'})
  put(d .. '/while-stopped.md', 'x')
  rex.sleep(1) -- the change settles and commits while the plugin is stopped
  H.hub('hub.admin.start', {plugin = 'watchfix'})
  wait('the plugin', function() return H.hub('hub.test.status', {}).plugins.watchfix.registered end)
  wait('a nudge to the new instance', function() return (wf('wf.nudges', {}).nudges.plugin or 0) >= 1 end)
  wait_kinds(w, 'while-stopped.md:created')
end)

test('a directory renamed away while a timer waits makes no removal', function()
  fresh()
  local d = dir('away')
  local w = ensure('away', d, {settle_ms = 1000})
  wait_state(w, 'active')
  put(d .. '/a.md', 'x')
  local p = wait_kinds(w, 'a.md:created')
  ack(w, p.next)
  sh('rm ' .. d .. '/a.md && mv ' .. d .. ' ' .. d .. '.away')
  wait_state(w, 'missing')
  rex.sleep(1.5)
  expect(kinds(read(w).rows)).to_be('')
  -- Back in place: the watch opens again (30 s retry), and a scan settles the real removal.
  sh('mv ' .. d .. '.away ' .. d)
  wait_kinds(w, 'a.md:removed', 45)
end)

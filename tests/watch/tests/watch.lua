-- hub.watch on the real hub and a real fsnotify watcher (docs/hub.md, "File watches").
-- The fixture plugin watchfix may watch LAB/watched (manifest test.watch_roots).

local root = H.args.lab .. '/watched'

local function wf(action, args)
  local st = H.hub('hub.test.status', {})
  return H.perform(st.plugins.watchfix.client_id, action, args or {})
end

-- call makes a hub call as the plugin and raises the hub's error with its code.
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

local function read(w, after)
  local a = {key = w.key, watch_id = w.watch_id}
  if after then a.after = after end
  return call('hub.watch.read', a)
end

local function ack(w, through) return call('hub.watch.ack', {key = w.key, watch_id = w.watch_id, through = through}) end

local function state(w)
  local list = call('hub.watch.list', {}).watches
  for _, x in ipairs(list) do
    if x.watch_id == w.watch_id then return x.state end
  end
end

-- kinds is "name:kind name:kind" in seq order.
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

-- rex.sleep is the only wait without a condition: it proves that nothing more arrives.
local function quiet(seconds) rex.sleep(seconds) end

test('a watch outside the plugin watch_roots is refused', function()
  fresh()
  sh('mkdir -p ' .. H.args.lab .. '/outside')
  expect_error(function() ensure('out', H.args.lab .. '/outside') end, 'watch_not_granted')
  expect_error(function() ensure('gone', root .. '/nope') end, 'watch_missing')
  expect_error(function() ensure('rec', dir('rec'), {recursive = true}) end, 'recursive')
  expect_error(function() ensure('Bad Key', dir('badkey')) end, 'key must match')
end)

test('a new file reads as created once, and an ack then a change reads as changed', function()
  fresh()
  local d = dir('basic')
  put(d .. '/old.md', 'before the watch')
  local w = ensure('basic', d)
  expect(w.state).to_be('initializing')
  wait_state(w, 'active')
  put(d .. '/new.md', 'one')
  local p = wait_kinds(w, 'new.md:created')
  expect(p.rows[1].path).to_be(d .. '/new.md')
  expect(p.rows[1].appearance_key).to_be(w.watch_id .. ':' .. p.rows[1].seq)
  ack(w, p.next)
  put(d .. '/new.md', 'two two')
  p = wait_kinds(w, 'new.md:changed')
  ack(w, p.next)
  -- The baseline file is hidden until it changes.
  put(d .. '/old.md', 'changed after the watch')
  p = wait_kinds(w, 'old.md:changed')
  ack(w, p.next)
  os.remove(d .. '/new.md')
  wait_kinds(w, 'new.md:removed')
end)

test('ensure is idempotent and a different spec for the key is a conflict', function()
  fresh()
  local d = dir('idem')
  local a = ensure('idem', d)
  local b = ensure('idem', d)
  expect(b.watch_id).to_be(a.watch_id)
  local e = expect_error(function() ensure('idem', d, {glob = '*.md'}) end, 'operation_conflict')
  expect(e.code).to_be('operation_conflict')
  -- raw is the Rex text with the full message (message drops the repeated code).
  expect(tostring(e.raw):find('operation_conflict: key idem has another spec; remove it first', 1, true) ~= nil,
    'the full message: ' .. tostring(e.raw)).to_be(true)
end)

test('an atomic save gives one row; hidden temp files give none', function()
  fresh()
  local d = dir('atomic')
  local w = ensure('atomic', d)
  wait_state(w, 'active')
  put(d .. '/.note.md.tmp', 'draft')
  sh('mv ' .. d .. '/.note.md.tmp ' .. d .. '/note.md')
  local p = wait_kinds(w, 'note.md:created')
  ack(w, p.next)
  put(d .. '/.note.md.tmp', 'second version')
  sh('mv ' .. d .. '/.note.md.tmp ' .. d .. '/note.md')
  p = wait_kinds(w, 'note.md:changed')
  ack(w, p.next)
  quiet(1)
  expect(kinds(read(w).rows)).to_be('')
end)

test('many writes to one file settle into one row', function()
  fresh()
  local d = dir('writes')
  local w = ensure('writes', d, {settle_ms = 500})
  wait_state(w, 'active')
  for i = 1, 5 do
    local f = assert(io.open(d .. '/log.md', 'a'))
    f:write('line ', i, '\n')
    f:close()
    rex.sleep(0.1)
  end
  wait_kinds(w, 'log.md:created')
  quiet(1)
  local p = read(w)
  expect(kinds(p.rows)).to_be('log.md:created')
  expect(p.rows[1].size).to_be('35')
end)

test('a glob limits the tracked names', function()
  fresh()
  local d = dir('glob')
  local w = ensure('glob', d, {glob = '*.md'})
  wait_state(w, 'active')
  put(d .. '/skip.txt', 'x')
  put(d .. '/take.md', 'x')
  wait_kinds(w, 'take.md:created')
end)

test('the plugin gets a nudge after a commit', function()
  fresh()
  local d = dir('nudge')
  local w = ensure('nudge', d)
  wait_state(w, 'active')
  put(d .. '/a.md', 'x')
  wait('a nudge', function() return (wf('wf.nudges', {}).nudges.nudge or 0) >= 1 end)
end)

test('a lost nudge is repeated while rows wait, with no later file change', function()
  fresh()
  local d = dir('lost')
  local w = ensure('lost', d)
  wait_state(w, 'active')
  local f = fault({point = 'watch-nudge', mode = 'drop', key = 'lost'})
  put(d .. '/a.md', 'x')
  wait('the dropped nudge', function() return fault_fired(f) end)
  expect(wf('wf.nudges', {}).nudges.lost).to_be(nil)
  -- The repeat comes 5 s later, without another change.
  wait('the repeated nudge', function() return (wf('wf.nudges', {}).nudges.lost or 0) >= 1 end, 15)
end)

test('remove takes the current watch_id only; a recreated key gets new ids and seqs', function()
  fresh()
  local d1, d2 = dir('re1'), dir('re2')
  local old = ensure('re', d1)
  wait_state(old, 'active')
  put(d1 .. '/a.md', 'x')
  local p = wait_kinds(old, 'a.md:created')
  expect(call('hub.watch.remove', {key = 're', watch_id = old.watch_id}).removed).to_be(true)
  -- A retry after a lost reply.
  expect(call('hub.watch.remove', {key = 're', watch_id = old.watch_id}).removed).to_be(false)
  local new = ensure('re', d2)
  expect(new.watch_id).not_to_be(old.watch_id)
  -- Late calls with the old watch_id do not touch the new watch.
  expect(call('hub.watch.remove', {key = 're', watch_id = old.watch_id}).removed).to_be(false)
  expect_error(function() read(old) end, 'stale_watch')
  expect_error(function() ack(old, p.next) end, 'stale_watch')
  wait_state(new, 'active')
  put(d2 .. '/a.md', 'x')
  local q = wait_kinds(new, 'a.md:created')
  expect(tonumber(q.rows[1].seq) > tonumber(p.rows[1].seq)).to_be(true)
  expect(q.rows[1].event_key).not_to_be(p.rows[1].event_key)
end)

test('an ack past the watermark is refused and changes nothing', function()
  fresh()
  local d = dir('cursor')
  local w = ensure('cursor', d)
  wait_state(w, 'active')
  put(d .. '/a.md', 'x')
  local p = wait_kinds(w, 'a.md:created')
  expect_error(function() ack(w, tostring(tonumber(p.watermark) + 1)) end, 'bad_cursor')
  expect(kinds(read(w).rows)).to_be('a.md:created')
  expect(ack(w, p.next).cursor).to_be(p.next)
  expect(ack(w, '0').cursor).to_be(p.next)
end)

test('a directory over the entry limit is refused before any watch opens', function()
  fresh()
  local d = dir('big')
  sh('cd ' .. d .. ' && i=0; while [ $i -lt 1001 ]; do : > .ignored-$i; i=$((i+1)); done')
  expect_error(function() ensure('big', d) end, 'watch_too_large')
end)

test('a watched directory replaced with a symlink outside the roots is not granted', function()
  fresh()
  local d = dir('swap')
  local w = ensure('swap', d)
  wait_state(w, 'active')
  sh('mkdir -p ' .. H.args.lab .. '/elsewhere && rm -rf ' .. d .. ' && ln -s ' .. H.args.lab .. '/elsewhere ' .. d)
  -- The hub can see the removal before the symlink exists: then the watch is missing, and the
  -- 30 s retry finds the symlink.
  wait_state(w, 'not_granted', 45)
  put(H.args.lab .. '/elsewhere/a.md', 'x')
  quiet(1)
  expect(kinds(read(w).rows)).to_be('')
end)

test('a regular file replaced with a symlink reads as removed', function()
  fresh()
  local d = dir('link')
  local w = ensure('link', d)
  wait_state(w, 'active')
  put(d .. '/a.md', 'x')
  local p = wait_kinds(w, 'a.md:created')
  ack(w, p.next)
  sh('rm ' .. d .. '/a.md && ln -s /etc/hosts ' .. d .. '/a.md')
  wait_kinds(w, 'a.md:removed')
end)

test('a slow writer keeps initialization open without blocking hub calls', function()
  fresh()
  local d = dir('slow')
  put(d .. '/growing.md', 'start\n')
  sh('(i=0; while [ $i -lt 8 ]; do echo more >> ' .. d .. '/growing.md; sleep 0.15; i=$((i+1)); done) >/dev/null 2>&1 &')
  local w = ensure('slow', d)
  expect(w.state).to_be('initializing')
  -- An unrelated hub call is answered while the file is still being written.
  expect(call('hub.watch.list', {}).watches).to_be_truthy()
  expect(state(w)).to_be('initializing')
  wait_state(w, 'active')
  -- The baseline file is hidden: nothing to read.
  expect(kinds(read(w).rows)).to_be('')
end)

test('emit_existing reads the files already there as created', function()
  fresh()
  local d = dir('existing')
  put(d .. '/a.md', 'x')
  local w = ensure('existing', d, {emit_existing = true})
  wait_kinds(w, 'a.md:created')
  wait_state(w, 'active')
end)

test('a watch that grows past the entry limit closes as too_large', function()
  fresh()
  local d = dir('grow')
  local w = ensure('grow', d)
  wait_state(w, 'active')
  sh('cd ' .. d .. ' && i=0; while [ $i -lt 1001 ]; do : > .ignored-$i; i=$((i+1)); done')
  wait_state(w, 'too_large')
  put(d .. '/a.md', 'x')
  rex.sleep(1)
  expect(kinds(read(w).rows)).to_be('')
end)

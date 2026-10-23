-- A small plugin that tests the Lua test harness itself (docs/testing.md, "Harness self-tests").
-- Its behavior is set by rows of its own SQL table kv(key, value), so a test changes it through the
-- real hub (value is JSON text):
--   mode = 'fail'  /home renders an error
--   mode = 'big'   /home renders a tree larger than one control message
--   mode = 'noparity'  /home has an action with neither tool nor user_only
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')

local function hub(action, args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if err or not res then
    -- Keep a transient code transient, so the hub retries a view operation (the gate fault test).
    local text = type(err) == 'table' and (err.message or '') or tostring(err)
    local code = type(err) == 'table' and err.code or text:match('code=([%w_%-%.]+)%)%s*$')
    error({code = code == 'unavailable' and 'unavailable' or 'hub_failed', message = text})
  end
  return res.result
end

-- get reads the kv row key. A render passes its render ID, so that the view tracks the table kv.
local function get(key, render)
  local row = hub('hub.sql.query', {sql = 'SELECT value FROM kv WHERE key = ?', params = {key}, render = render}).rows[1]
  return row and rex.json.decode(row[1])
end

local function put(key, value)
  return hub('hub.sql.exec', {sql = 'INSERT INTO kv VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value',
    params = {key, rex.json.encode(value)}})
end

-- send_parts sends a reply in parts, as the SDK does (docs/hub.md "Reply parts"), and returns the
-- stub. A slice ends at a UTF-8 character boundary. The real encoded hub.reply.part request is the
-- check: at most 60 KiB, else the slice shrinks in proportion and is encoded again. mode 'gap'
-- skips the second part; 'bytes' sends a wrong byte count.
local part_seq = 0
local function send_parts(value, mode)
  local text = rex.json.encode(value)
  part_seq = part_seq + 1
  local id = 'fx-' .. instance_id:gsub('[^%w._:-]', '') .. '-' .. part_seq
  local slices, i = {}, 1
  while i <= #text do
    local j = math.min(i + 57343, #text)
    while true do
      while j < #text and text:byte(j + 1) >= 0x80 and text:byte(j + 1) < 0xC0 do j = j - 1 end
      local size = #rex.json.encode({client_id = hub_id, action = 'hub.reply.part', timeout_ms = 10000,
        args = {id = id, index = 64, count = 64, data = text:sub(i, j)}})
      if size <= 60 * 1024 then break end
      j = i + math.floor((j - i) * (58 * 1024) / size)
    end
    slices[#slices + 1] = text:sub(i, j)
    i = j + 1
  end
  for k, s in ipairs(slices) do
    if not (mode == 'gap' and k == 2) then
      hub('hub.reply.part', {id = id, index = k - 1, count = #slices, data = s})
    end
  end
  return {lazyrex_parts = {id = id, count = #slices, bytes = mode == 'bytes' and #text - 1 or #text}}
end

-- filler returns about kb KiB of view nodes n1..nN, with quotes, backslashes, <, & and multibyte
-- characters in each text.
local function filler(kb)
  local text = string.rep('aé"\\<&日', 60)
  local per = #rex.json.encode({id = 'n1000', type = 'widget', text = text}) + 1
  local nodes = {}
  for k = 1, math.floor(kb * 1024 / per) do nodes[k] = {id = 'n' .. k, type = 'widget', text = text} end
  return nodes
end

local obj = {type = 'object'}
local exports = {'fix.put', 'fix.get', 'fix.who', 'fix.fill', 'fix.echo', 'fix.invalidate', 'fix.spawn', 'fix.grant', 'fix.hub',
  'fix.measure', 'fix.register', 'fix.big', 'fix.saved', 'fix.notify'}
local actions = {
  {name = 'hub.forward', kind = 'client', title = 'Forwarded agent call', args = obj},
  {name = 'view.render', kind = 'client', title = 'Render', args = obj},
  {name = 'view.action', kind = 'client', title = 'Action', args = obj},
  {name = 'view.open', kind = 'client', title = 'Open a lazy tab', args = obj},
  {name = 'view.seen', kind = 'client', title = 'The user looks at a view', args = obj},
  {name = 'view.rename', kind = 'client', title = 'The user renamed a tab', args = obj},
  {name = 'fix.title', kind = 'client', title = 'Declare a tab with a new title, as an unread count does',
    args = {type = 'object', properties = {key = {type = 'string'}, title = {type = 'string'}}, required = {'key', 'title'}}},
  {name = 'fix.put', kind = 'client', title = 'Put a kv row', description = 'Write {key, value}.',
    args = {type = 'object', properties = {key = {type = 'string'}, value = {}}, required = {'key', 'value'}}},
  {name = 'fix.get', kind = 'client', title = 'Get a kv row', args = {type = 'object', properties = {key = {type = 'string'}}, required = {'key'}}},
  {name = 'fix.who', kind = 'client', title = 'Who is calling', args = obj},
  {name = 'fix.fill', kind = 'client', title = 'Write a kv row of n copies of text',
    args = {type = 'object', properties = {key = {type = 'string'}, text = {type = 'string'}, n = {type = 'integer'}}, required = {'key', 'text', 'n'}}},
  {name = 'fix.echo', kind = 'client', title = 'Echo', args = obj},
  {name = 'fix.big', kind = 'client', title = 'A reply of kb KiB, sent in parts',
    args = {type = 'object', properties = {kb = {type = 'integer'}}, required = {'kb'}}},
  {name = 'fix.saved', kind = 'client', title = 'A keyed write with a saved reply of kb KiB, and its replays',
    args = {type = 'object', properties = {kb = {type = 'integer'}, key = {type = 'string'}, step = {type = 'string'}}, required = {'kb', 'key', 'step'}}},
  {name = 'fix.notify', kind = 'client', title = 'Commit notices with hub.sql.commit; returns {} or {error = {code, message}}',
    args = {type = 'object', properties = {notices = {type = 'array'}}, required = {'notices'}}},
  {name = 'fix.invalidate', kind = 'client', title = 'Call hub.ui.invalidate', args = obj},
  {name = 'fix.grant', kind = 'client', title = 'Make a grant for the caller (on_behalf = as_pid or the caller pid)',
    args = {type = 'object', properties = {key = {type = 'string'}, entries = {type = 'array'}, as_pid = {type = 'integer'}}, required = {'key', 'entries'}}},
  -- Direct calls only (not exports): deferred work of the plugin, outside any forward.
  {name = 'fix.grant_spawn', kind = 'client', title = 'hub.spawn_window with a grant', args = obj},
  {name = 'fix.grant_revoke', kind = 'client', title = 'hub.grant.revoke', args = obj},
  {name = 'fix.grant_get', kind = 'client', title = 'hub.grant.get', args = obj},
  {name = 'fix.ensure', kind = 'client', title = 'hub.session.ensure', args = obj},
  {name = 'fix.select', kind = 'client', title = 'hub.app.select_session', args = obj},
  {name = 'fix.window_later', kind = 'client', title = 'hub.spawn_window for the user from rex.after; the outcome goes to the kv row window/<key>', args = obj},
  {name = 'fix.spawn', kind = 'client', title = 'Start a shell on behalf of the caller (on_behalf = as_pid or the caller pid)',
    args = {type = 'object', properties = {key = {type = 'string'}, as_pid = {type = 'integer'}}, required = {'key'}}},
  {name = 'fix.hub', kind = 'client', title = 'Call a hub action as the plugin', description = 'Returns {result} or {error = {code, message}}.',
    args = {type = 'object', properties = {action = {type = 'string'}, args = {type = 'object'}}, required = {'action'}}},
  {name = 'fix.measure', kind = 'client', title = 'Call a hub action as the plugin; return sizes, not the reply',
    args = {type = 'object', properties = {action = {type = 'string'}, args = {type = 'object'}, timeout_ms = {type = 'integer'}}, required = {'action'}}},
  {name = 'fix.register', kind = 'client', title = 'Register the plugin again', args = obj},
}
rex.call('client.keymap', {actions = actions})

local function who(pid)
  local r = hub('hub.identify', {pid = pid})
  if not r.found then error({code = 'unknown_caller', message = 'the hub does not recognize the caller'}) end
  return r.agent
end

-- register registers the plugin. fix.register calls it again (render_deps.lua: a registration
-- clears the declarations).
local function register()
  return hub('hub.register', {plugin = 'harnessfix', instance_id = instance_id, exports = exports,
    commands = {{id = 'fix_form', title = 'Fixture form', route = '/forms/fix', action = 'fix.put'}}})
end

local function domain(action, args, pid)
  if action == 'fix.put' then
    put(args.key, args.value)
    return {ok = true, by = who(pid)}
  elseif action == 'fix.get' then
    return {value = get(args.key)}
  elseif action == 'fix.who' then
    return {agent = who(pid)}
  elseif action == 'fix.fill' then
    put(args.key, {text = string.rep(args.text, args.n)})
    return {ok = true}
  elseif action == 'fix.echo' then
    return args
  elseif action == 'fix.invalidate' then
    return hub('hub.ui.invalidate', {})
  elseif action == 'fix.grant' then
    return hub('hub.grant.create', {on_behalf = args.as_pid or pid, key = args.key, entries = args.entries})
  elseif action == 'fix.grant_spawn' then
    return hub('hub.spawn_window', args)
  elseif action == 'fix.grant_revoke' then
    return hub('hub.grant.revoke', args)
  elseif action == 'fix.grant_get' then
    return hub('hub.grant.get', args)
  elseif action == 'fix.ensure' then
    return hub('hub.session.ensure', args)
  elseif action == 'fix.select' then
    return hub('hub.app.select_session', args)
  elseif action == 'fix.window_later' then
    -- Deferred work, as a plugin's own timer does it: the reply does not wait for the window.
    rex.after(0, function()
      local ok, r = pcall(hub, 'hub.spawn_window', {['for'] = 'user', key = args.key, session = args.session, layout = args.layout})
      put('window/' .. args.key, ok and {result = r} or {error = type(r) == 'table' and r.message or tostring(r)})
    end)
    return {scheduled = true}
  elseif action == 'fix.spawn' then
    return {spawn = hub('hub.spawn', {profile = 'shell', key = args.key, on_behalf = args.as_pid or pid}), pid = pid}
  elseif action == 'fix.big' then
    return send_parts({kb = args.kb, data = string.rep('b', args.kb * 1024)})
  elseif action == 'fix.saved' then
    -- A keyed write whose reply of kb KiB the hub saves (docs/sql.md "Saved replies"). Steps:
    --   write   inserts a row and commits with reply_parts, then answers {withheld = true} in place
    --           of the reply. No transport reply is lost: it is a probe, not a fault.
    --   commit  repeats the write with the same key: the commit replays the saved reply.
    --   replay  reads the saved reply with hub.sql.replay.
    --   deliver reads it with hub.sql.replay and sends it to the caller in parts.
    -- commit and replay answer only the size of what arrived and whether it is the saved value.
    local value = {kb = args.kb, data = string.rep('s', args.kb * 1024)}
    local function check(reply)
      return {bytes = #rex.json.encode(reply), same = type(reply) == 'table' and reply.kb == value.kb and reply.data == value.data}
    end
    if args.step == 'write' or args.step == 'commit' then
      local tx = hub('hub.sql.begin', {}).tx
      hub('hub.sql.exec', {tx = tx, sql = 'INSERT INTO saved_rows VALUES (?)', params = {args.key}})
      local commit = {tx = tx, key = args.key, fingerprint = 'f'}
      if args.step == 'write' then commit.reply_parts = send_parts(value).lazyrex_parts end
      local r = hub('hub.sql.commit', commit)
      if args.step == 'write' then return {withheld = true, replayed = r.replayed == true} end
      local out = check(r.reply)
      out.replayed = r.replayed == true
      return out
    end
    local r = hub('hub.sql.replay', {key = args.key, fingerprint = 'f'})
    if args.step == 'replay' then
      local out = check(r.reply)
      out.found = r.found
      return out
    end
    return send_parts(r.reply)
  elseif action == 'fix.notify' then
    -- Notices go in a hub.sql.commit (docs/sql.md, section 7). The commit's answer as data.
    local tx = hub('hub.sql.begin', {}).tx
    local _, err = rex.call('client.perform', {client_id = hub_id, action = 'hub.sql.commit', args = {tx = tx, notices = args.notices}, timeout_ms = 10000})
    if err then
      local text = type(err) == 'table' and (err.message or '') or tostring(err)
      local code = type(err) == 'table' and err.code or text:match('code=([%w_%-%.]+)%)%s*$')
      return {error = {code = code, message = text}}
    end
    return {}
  elseif action == 'fix.register' then
    register()
    return {}
  elseif action == 'fix.hub' then
    -- The hub's answer as data, so a test can check the exact error code.
    local res, err = rex.call('client.perform', {client_id = hub_id, action = args.action, args = args.args or {}, timeout_ms = 10000})
    if err then
      local text = type(err) == 'table' and (err.message or '') or tostring(err)
      local code = type(err) == 'table' and err.code or tostring(err):match('code=([%w_%-%.]+)%)%s*$')
      return {error = {code = code, message = text}}
    end
    return {result = res and res.result}
  elseif action == 'fix.measure' then
    -- The reply crosses the real transport (hub -> Rex -> this plugin). The test gets only its
    -- shape and sizes: lua_bytes is rex.json.encode of what arrived (Rex's encoding, not Go's).
    local res, err = rex.call('client.perform', {client_id = hub_id, action = args.action, args = args.args or {}, timeout_ms = args.timeout_ms or 10000})
    if err then
      local text = type(err) == 'table' and (err.message or '') or tostring(err)
      local code = type(err) == 'table' and err.code or text:match('code=([%w_%-%.]+)%)%s*$')
      return {arrived = false, code = code, message_bytes = #text, message = text:sub(1, 600)}
    end
    local r = res and res.result or {}
    return {arrived = true, lua_bytes = #rex.json.encode(r), envelope_bytes = #rex.json.encode(res)}
  end
  error({code = 'not_supported', message = 'unknown action ' .. tostring(action)})
end

local function home_tree()
  local mode = get('mode')
  if mode == 'fail' then error({code = 'render_error', message = 'mode is fail'}) end
  local nodes = {
    {id = 'status', type = 'widget', text = 'mode ' .. tostring(mode or 'normal'),
      action = {name = 'refresh', label = 'Refresh', enabled = true, user_only = 'redraws the view'}},
    {id = 'open_form', type = 'widget', text = 'Open the form',
      action = {name = 'open', label = 'Open', enabled = true, user_only = 'navigation'}},
    {id = 'count', type = 'widget', text = 'Count calls',
      action = {name = 'count', label = 'Count', enabled = true, tool = 'fix.put'}},
    {id = 'bad_nav', type = 'widget', text = 'Navigate to a missing tab',
      action = {name = 'go', label = 'Go', enabled = true, user_only = 'navigation'}},
    {id = 'cross_nav', type = 'widget', text = 'Navigate to the tab fixture',
      action = {name = 'go', label = 'Go', enabled = true, user_only = 'navigation'}},
    {id = 'cross_missing', type = 'widget', text = 'Navigate to a plugin that is not there',
      action = {name = 'go', label = 'Go', enabled = true, user_only = 'navigation'}},
    {id = 'bad_target', type = 'widget', text = 'A malformed navigate effect (input.kind picks it)',
      action = {name = 'go', label = 'Go', enabled = true, user_only = 'navigation'}},
    {id = 'gated_nav', type = 'widget', text = 'Navigate to a tab that view.open declares only when gate is set',
      action = {name = 'go', label = 'Go', enabled = true, user_only = 'navigation'}},
  }
  if mode == 'noparity' then
    nodes[#nodes + 1] = {id = 'bare', type = 'widget', text = 'no parity', action = {name = 'poke', label = 'Poke', enabled = true}}
  end
  if mode == 'big' then
    nodes[#nodes + 1] = {id = 'big', type = 'widget', text = string.rep('x', 70000)}
  end
  return {title = 'Fixture', back = nil, nodes = nodes}
end

local function form_tree()
  return {title = 'Fixture form', nodes = {
    {id = 'form', type = 'form', fields = {{id = 'value', label = 'Value', kind = 'text', value = ''}},
      submit_action = {name = 'submit', label = 'Save', enabled = true, tool = 'fix.put'}},
    {id = 'go_home', type = 'widget', text = 'Tab fixture home', action = {name = 'go', label = 'Go', enabled = true, user_only = 'navigation'}},
    {id = 'chain', type = 'widget', text = 'Chain', action = {name = 'chain', label = 'Chain', enabled = true, user_only = 'test: opens another form'}},
    {id = 'bad_form', type = 'widget', text = 'Bad form', action = {name = 'bad', label = 'Bad', enabled = true, user_only = 'test: a failing effect'}},
    {id = 'cancel', type = 'widget', text = 'Cancel', action = {name = '$cancel', label = 'Cancel', enabled = true}},
  }}
end

-- Lazy tabs (/lazy/<name>) are declared on view.open, as the messaging plugin declares an empty
-- direct thread. The declaration calls the hub while the hub waits for view.open: the case of
-- rex-protocol.md, "The hub has one request queue". /lazy/slow waits 2 s first; /nope/* declares
-- nothing. view.open counts its calls in the table opens (one row for each call), not in kv: a
-- view of /lazy/deps-<x> tracks kv.
local tabs = {{key = 'home', title = 'Fixture home', route = '/home'}}

local function view_open(args)
  hub('hub.sql.exec', {sql = 'INSERT INTO opens VALUES (?)', params = {args.route}})
  local gated = args.route:match('^/gated/(.+)$')
  if gated and get('gate') then
    tabs[#tabs + 1] = {key = 'gated/' .. gated, title = 'Gated ' .. gated, route = args.route}
    hub('hub.ui.tabs', {tabs = tabs})
    return {}
  end
  local name = args.route:match('^/lazy/(.+)$')
  if not name then return {} end
  if name == 'slow' then rex.sleep(2) end
  tabs[#tabs + 1] = {key = 'lazy/' .. name, title = 'Lazy ' .. name, route = args.route}
  hub('hub.ui.tabs', {tabs = tabs})
  return {}
end

-- retitle declares a tab with a new title, as the messaging plugin does for an unread count. The hub
-- then writes the new label: that label is not a user rename.
local function retitle(args)
  for _, t in ipairs(tabs) do
    if t.key == args.key then t.title = args.title end
  end
  hub('hub.ui.tabs', {tabs = tabs})
  return {}
end

local function view(action, args)
  if action == 'view.open' then return view_open(args) end
  -- view.rename: the plugin keeps the user's title literally and declares it again, as the messaging
  -- plugin does. The kv row renames lists every title in order.
  if action == 'view.rename' then
    for _, t in ipairs(tabs) do
      if t.route == args.route then t.title = args.title end
    end
    local list = get('renames') or {}
    list[#list + 1] = args.title
    put('renames', list)
    hub('hub.ui.tabs', {tabs = tabs})
    return {}
  end
  if action == 'view.seen' then put('seen', {route = args.route, reason = args.reason}); return {} end
  if action == 'view.render' then
    if args.route == '/home' then return {tree = home_tree()} end
    if args.route == '/forms/fix' or args.route == '/forms/chain' then return {tree = form_tree()} end
    -- /lazy/deps-<x> (render_deps.lua): the kv row spec/deps-<x> is the deps declaration of the
    -- reply ('fail' fails the render); the text shows the kv row data/deps-<x>. The reads carry
    -- the render ID, so the view tracks the table kv.
    local dname = args.route:match('^/lazy/(deps%-.+)$')
    if dname then
      local spec = get('spec/' .. dname, args.render)
      if spec == 'fail' then error({code = 'render_error', message = 'spec is fail'}) end
      return {tree = {title = 'Deps', nodes = {{id = 'lazy', type = 'widget', text = tostring(get('data/' .. dname, args.render)),
        action = {name = 'poke', label = 'Poke', enabled = true, user_only = 'changes nothing'}}}}, deps = spec}
    end
    -- /lazy/reads-<x> (render_reads.lua): the SQL row reads_spec(<x>) gives the refresh value of the
    -- reply and the mode of the data read: 'tokened' (the query carries args.render), 'untokened'
    -- (no render), 'other' (a tokened query, then another hub action), 'refused' (a tokened query,
    -- then a tokened query with tx that the hub refuses). The text counts the rows of the table reads_<x>.
    local rname = args.route:match('^/lazy/reads%-(%w+)$')
    if rname then
      local function query(sql, tokened)
        local a = {sql = sql, params = {rname}}
        if tokened then a.render = args.render end
        return hub('hub.sql.query', a).rows[1]
      end
      local spec = query('SELECT refresh, mode FROM reads_spec WHERE name = ?', true) or {}
      local refresh, mode = spec[1], spec[2]
      local n = query('SELECT count(*) FROM reads_' .. rname .. ' WHERE ? IS NOT NULL', mode ~= 'untokened')
      if mode == 'other' then hub('hub.identify', {pid = 0}) end
      if mode == 'refused' then
        -- A tokened query that the hub refuses (tx with render). rex.call returns the error; the
        -- render goes on and still answers refresh: the hub must untrack it.
        rex.call('client.perform', {client_id = hub_id, action = 'hub.sql.query', timeout_ms = 10000,
          args = {sql = 'SELECT 1', render = args.render, tx = 'x'}})
      end
      return {tree = {title = 'Reads', nodes = {{id = 'lazy', type = 'widget', text = tostring(n and n[1])}}},
        refresh = type(refresh) == 'string' and refresh or nil}
    end
    -- /lazy/parts-<x> (reply_parts.lua): the SQL row parts_spec(<x>) gives kb, the mode of the parts
    -- ('ok', 'gap', 'bytes', 'inline'). The reply has no refresh field: the view is tracked. Over
    -- 44 KiB the reply goes in parts.
    -- The node count shows the rows of the table parts_<x>; the node last ends the tree.
    local pname = args.route:match('^/lazy/parts%-(%w+)$')
    if pname then
      local function query(sql)
        return hub('hub.sql.query', {sql = sql, params = {pname}, render = args.render}).rows[1]
      end
      local spec = query('SELECT kb, mode FROM parts_spec WHERE name = ?') or {}
      local n = query('SELECT count(*) FROM parts_' .. pname .. ' WHERE ? IS NOT NULL')
      local nodes = spec[2] == 'ascii' and {{id = 'big', type = 'widget', text = ''}} or filler(spec[1] or 1)
      nodes[#nodes + 1] = {id = 'count', type = 'widget', text = tostring(n and n[1])}
      nodes[#nodes + 1] = {id = 'last', type = 'widget', text = 'end'}
      local reply = {tree = {title = 'Parts', nodes = nodes}}
      if spec[2] == 'ascii' then -- an ASCII reply of exactly 50000 bytes: one part
        nodes[1].text = string.rep('a', 50000 - #rex.json.encode(reply))
      end
      if spec[2] == 'inline' or #rex.json.encode(reply) <= 45056 then return reply end
      return send_parts(reply, spec[2])
    end
    if args.route:match('^/lazy/') then return {tree = {title = 'Lazy', nodes = {{id = 'lazy', type = 'widget', text = args.route}}}} end
    return {tree = {title = 'Unknown', nodes = {{id = 'unknown', type = 'error', text = 'no route ' .. tostring(args.route)}}}}
  elseif action == 'view.action' then
    if args.route == '/home' and args.node_id == 'open_form' then return {form = '/forms/fix'} end
    if args.route == '/home' and args.node_id == 'bad_nav' then return {navigate = 'missing'} end
    if args.route == '/home' and args.node_id == 'status' then return {} end
    if args.route == '/home' and args.node_id == 'count' then
      -- Counts the plugin's calls for an operation (views.lua: a final operation is not run again).
      local n = get('count') or 0
      put('count', n + 1)
      return {count = n + 1}
    end
    if args.route == '/home' and args.node_id == 'cross_nav' then return {navigate = {plugin = 'tabfix', tab = 'home'}} end
    if args.route == '/home' and args.node_id == 'cross_missing' then return {navigate = {plugin = 'nope', tab = 'x'}} end
    if args.route == '/home' and args.node_id == 'bad_target' then
      local kind = args.input and args.input.kind
      local targets = {tab_number = {plugin = 'lazyrex', tab = 7}, plugin_number = {plugin = 7, tab = 'home'},
        boolean = true, empty_string = '', empty_table = {}, no_tab = {plugin = 'lazyrex'}}
      return {navigate = targets[kind]}
    end
    if args.route == '/home' and args.node_id == 'gated_nav' then return {navigate = {plugin = 'harnessfix', tab = 'gated/a'}} end
    if args.route:match('^/lazy/deps%-') and args.node_id == 'lazy' then return {} end -- the tree stays the same
    if args.route == '/forms/fix' and args.node_id == 'go_home' then return {navigate = {plugin = 'tabfix', tab = 'home'}} end
    -- views.lua: a form effect from a form (a chained form), and an effect that fails.
    if args.route == '/forms/fix' and args.node_id == 'chain' then return {form = '/forms/chain'} end
    if args.route == '/forms/fix' and args.node_id == 'bad_form' then return {form = 'nope'} end
    if args.route == '/forms/fix' and args.node_id == 'form' then
      put('form', {value = args.input and args.input.values and args.input.values.value or ''})
      return {close_form = true}
    end
    error({code = 'invalid_input', message = 'unknown node ' .. tostring(args.node_id)})
  end
end

rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if ev.action == 'hub.forward' or ev.action:sub(1, 5) == 'view.' then
      if ev.requester.client_id ~= hub_id then error({code = 'refused', message = 'Only the hub may call ' .. ev.action}) end
    end
    if ev.action == 'hub.forward' then
      return domain(ev.args.action, ev.args.args or {}, ev.args.requester_pid)
    elseif ev.action:sub(1, 5) == 'view.' then
      return view(ev.action, ev.args)
    end
    if ev.action == 'fix.title' then return retitle(ev.args) end
    return domain(ev.action, ev.args or {}, ev.requester.principal and ev.requester.principal.pid)
  end)
  if ok then
    rex.call('client.reply', {call_id = ev.call_id, result = result})
  else
    local e = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}
    rex.call('client.reply', {call_id = ev.call_id, error = e})
  end
end)

register()
hub('hub.sql.exec', {sql = 'CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT NOT NULL)'})
hub('hub.sql.exec', {sql = 'CREATE TABLE IF NOT EXISTS opens (route TEXT NOT NULL)'})
hub('hub.ui.tabs', {tabs = tabs})
rex.call('client.set_label', {label = 'lazyrex-plugin:harnessfix'})

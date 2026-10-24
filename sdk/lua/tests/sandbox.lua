-- A sandbox for the SDK tests: a plugin main file and lazyrex.lua, loaded with a stub rex and the
-- fake hub.sql (fake_sql.lua). Nothing reaches a Rex server or a hub.
--   local s = sandbox(sdk_dir, base_env, {main = <path> | source = <Lua text>, args = {...},
--     hub = {[action] = function(args) return result, err end}, rex = {[method] = fn}})
-- s.loaded, s.load_error: the result of the main file. s:start() runs the timers (registration).
-- s:call(action, args, requester): a client.perform from the hub (default) or from an agent pid.
-- s:agent(name, args, pid): an agent action through hub.forward. s:render(route), s:select(...).
-- s.sql is the fake hub.sql; s.keymap, s.registered and s.replies record what the plugin sent.
return function(sdk_dir, base, opts)
  opts = opts or {}
  local s = {timers = {}, events = {}, replies = {}, hub_calls = {}, parts = {}, part_sets = {}, printed = {}, agents = opts.agents or {[7] = 'alice', [8] = 'bob'}}
  s.sql = dofile(sdk_dir .. '/tests/fake_sql.lua')()
  -- A commit with reply_parts saves the joined set, as the hub does.
  s.sql.resolve_parts = function(rp) return rex.json.decode(table.concat(s.part_sets[rp.id] or {})) end
  local args = {hub_client_id = 'hub', instance_id = 'todo-1', plugin_session = 'session-1'}
  for k, v in pairs(opts.args or {}) do args[k] = v end
  local function hub_call(action, a)
    s.hub_calls[#s.hub_calls + 1] = action
    local limits = opts.limits ~= false and (opts.limits or {inline_max = 45056, part_bytes = 57344, max_total = 786432}) or nil
    if opts.hub and opts.hub[action] then
      local r, e = opts.hub[action](a)
      -- An override of hub.register still gets the limits, as the hub sends them.
      if action == 'hub.register' and type(r) == 'table' and r.limits == nil then r.limits = limits end
      return r, e
    end
    if action:sub(1, 8) == 'hub.sql.' then return s.sql.handle(action, a) end
    -- The limits are the hub's (chunking v1.1); opts.limits replaces them, opts.limits = false sends none.
    if action == 'hub.register' then
      s.registered = a
      return {limits = limits}
    end
    -- hub.reply.part: s.parts records each call; s.part_sets keeps the data of each set by index.
    if action == 'hub.reply.part' then
      s.parts[#s.parts + 1] = a
      s.part_sets[a.id] = s.part_sets[a.id] or {}
      s.part_sets[a.id][a.index + 1] = a.data
      return {}
    end
    if action == 'hub.identify' then
      local agent = s.agents[a.pid]
      if not agent then return {found = false} end
      return {found = true, agent = agent, identity = agent, session_id = 'session-' .. agent, block_id = 'block-' .. agent, spawn_id = opts.spawns and opts.spawns[agent]}
    end
    if action == 'hub.ui.invalidate' then s.invalidated = (s.invalidated or 0) + 1; return {} end
    return nil, {code = 'not_supported', message = 'sandbox hub: ' .. action}
  end
  local stub = {
    args = args,
    session_id = 'session-1',
    json = rex.json,
    on = function(name, fn) s.events[name] = fn end,
    after = function(_, fn) s.timers[#s.timers + 1] = fn end,
    call = function(method, a)
      if opts.rex and opts.rex[method] then return opts.rex[method](a) end
      if method == 'client.perform' then
        local result, err = hub_call(a.action, a.args)
        if result == nil then
          -- As Rex: an action error is text that ends with the status and the code.
          local e = err or {code = 'internal', message = 'no result'}
          return nil, 'client.perform failed: ' .. e.code .. ': ' .. tostring(e.message) .. ' (status=500 code=internal)'
        end
        return {result = result}
      end
      if method == 'client.keymap' then s.keymap = a.actions; return {} end
      if method == 'client.reply' then s.replies[#s.replies + 1] = a; s.reply = a; return {} end
      if method == 'client.set_label' then s.label = a.label; return {} end
      return {}
    end,
  }
  -- The other Rex calls that read outside state (lazyrex.lua, "External reads in a render"). Each one
  -- records its name in s.rex_calls and answers {name = <name>}.
  s.rex_calls = {}
  local function rec(name) return function() s.rex_calls[#s.rex_calls + 1] = name; return {name = name} end end
  local function api(prefix)
    return {call = rec(prefix .. 'call'), block = {call = rec(prefix .. 'block.call')}, session = {view = rec(prefix .. 'session.view')},
      exec = setmetatable({start = rec(prefix .. 'exec.start')}, {__call = function(_, ...) return rec(prefix .. 'exec')(...) end})}
  end
  local own = api('')
  stub.block, stub.session = own.block, own.session
  stub.invoke, stub.wait, stub.servers, stub.host = rec('invoke'), rec('wait'), rec('servers'), rec('host')
  stub.exec = setmetatable({start = rec('exec.start')}, {__call = function(_, ...) return rec('exec')(...) end})
  -- rex.server raises for an unknown label (Rex docs). A second result checks that the SDK passes all.
  stub.server = function(label)
    if label == 'bad' then error('unknown server bad', 0) end
    s.rex_calls[#s.rex_calls + 1] = 'server'
    return api(label .. ':'), 'second'
  end
  -- opts.frozen: a rex table that keeps its fields (the SDK cannot replace them).
  local rex_env = stub
  if opts.frozen then rex_env = setmetatable({}, {__index = stub, __newindex = function() end}) end
  -- Own io and os tables: the SDK replaces io.open and the others in them, not in the test runner.
  local env = setmetatable({rex = rex_env, io = setmetatable({}, {__index = io}), os = setmetatable({exit = function(code) s.exited = code end}, {__index = os}), print = function(...) s.printed[#s.printed + 1] = table.concat({...}, ' ') end}, {__index = base})
  s.env = env -- a test can set s.env.os.time (the peer clock)
  local sdk_chunk = assert(loadfile(sdk_dir .. '/lazyrex.lua'))
  setfenv(sdk_chunk, env)
  local sdk = sdk_chunk()
  s.sdk = sdk
  env.require = function(name) if name == 'lazyrex' then return sdk end return base.require(name) end
  local chunk, err
  if opts.source then chunk, err = loadstring(opts.source, '=main.lua') else chunk, err = loadfile(opts.main) end
  if not chunk then s.loaded, s.load_error = false, err; return s end
  setfenv(chunk, env)
  s.loaded, s.load_error = pcall(chunk)
  -- start runs the timers that are due (the registration first).
  function s:tick()
    local due = self.timers
    self.timers = {}
    for _, fn in ipairs(due) do fn() end
  end
  s.start = s.tick
  function s:call(action, a, pid)
    self.reply = nil
    local requester = pid and {client_id = 'agent-client', principal = {kind = 'peer', pid = pid}} or {client_id = 'hub'}
    self.events.client_perform_requested({}, {requester = requester, action = action, call_id = 'c' .. #self.replies, args = a})
    return self.reply
  end
  function s:agent(name, a, pid) return self:call('hub.forward', {action = name, args = a or {}, requester_pid = pid or 7}) end
  function s:render(route, render) return self:call('view.render', {route = route, user = 'user', render = render or 'r1'}) end
  function s:select(route, node_id, item_id, key)
    return self:call('view.action', {route = route, node_id = node_id, action = 'select', input = {item_id = item_id}, user = 'user', operation_key = key or 'op1'})
  end
  return s
end

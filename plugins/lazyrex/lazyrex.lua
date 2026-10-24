-- LazyRex home. The hub owns profiles, workers, and navigation. Home has no tab: its palette
-- entries are "Agents…" and "Plugins…" (panels), "Stop Worker…" and, while no other plugin offers
-- a launch, "Launch Profile…" (forms).
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')
local navigation_notice
local function fail(code, message) error({code = code, message = message}) end
-- A hub error keeps its code. Only an error without a code, or with a transient code, becomes
-- 'unavailable' (retryable): a definite refusal must end an operation, not repeat it.
local transient = {unavailable = true, disconnected = true, timeout = true}
-- hub_error turns a Rex perform error into code, message. Rex Lua gives it as text that ends in
-- "(status=<n> code=<code>)" (docs/rex-protocol.md).
local function hub_error(call_error)
  local code, message
  if type(call_error) == 'table' then
    code, message = call_error.code, call_error.message
  else
    local text = tostring(call_error or 'Hub request failed')
    code = text:match('code=([%w_%-%.]+)%)%s*$')
    message = text:match('failed: (.*) %(status=%d+ code=[^)]*%)%s*$') or text
    -- A hub error message keeps its own "code: " prefix; drop it when it repeats the code.
    if code and message:sub(1, #code + 2) == code .. ': ' then message = message:sub(#code + 3) end
  end
  if not code or transient[code] then code = 'unavailable' end
  return code, message or 'Hub request failed'
end
local function hub(action, args)
  local response, call_error = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if type(response) ~= 'table' then fail(hub_error(call_error)) end
  if type(response.result) ~= 'table' then fail('internal', 'Invalid hub result') end
  return response.result
end
-- read is a covered hub read of a render (docs/hub.md, "Hub reads in a render"): the render ID
-- keeps the render tracked. A render passes its own ID; no other code borrows it. An action passes
-- nil: then it is a plain hub call.
local function read(render_id, action, args)
  local a = {render = render_id}
  for k, v in pairs(args or {}) do a[k] = v end
  return hub(action, a)
end
-- set_notice changes the launch notice. The notice is Lua state that the Agents… panel shows, not hub
-- data, so a change renders the open panel again (hub.ui.invalidate). A failure is ignored, as the
-- SDK app.refresh does; the call raises nothing.
local function set_notice(text)
  if text == navigation_notice then return end
  navigation_notice = text
  rex.call('client.perform', {client_id = hub_id, action = 'hub.ui.invalidate', args = {routes = {'/panels/agents'}}, timeout_ms = 10000})
end
-- Every user action names its agent tool, or why it is user-only (docs/testing.md, "Agent parity").
-- Navigation changes only what the user sees; agents start and stop workers with the hub tools.
local function action(name, label, parity)
  local a = {name = name, label = label, enabled = true}
  for k, v in pairs(parity) do a[k] = v end
  return a
end
local navigation = {user_only = 'navigation'}
local function cancel_node(label)
  return {id = 'cancel', type = 'widget', text = '', action = {name = '$cancel', label = label or 'Cancel', enabled = true}}
end
local function find(list, field, value)
  for _, item in ipairs(list or {}) do if item[field] == value then return item end end
  fail('not_found', 'The selected item is no longer available. Refresh the view.')
end
local function active(worker)
  return worker.state == 'starting' or worker.state == 'running' or worker.state == 'exited'
end
local function worker_label(worker) return worker.title or worker.agent_id or worker.spawn_id end
local function worker_detail(worker)
  local detail = worker.state or 'Unknown'
  if type(worker.status) == 'table' then
    detail = worker.status.state or detail
    if type(worker.status.msg) == 'string' and worker.status.msg ~= '' then detail = detail .. ' · ' .. worker.status.msg end
  end
  if type(worker.error) == 'string' and worker.error ~= '' then detail = detail .. ' · ' .. worker.error end
  return detail
end
local function registered(id, render_id)
  for _, plugin in ipairs(read(render_id, 'hub.plugins').plugins or {}) do if plugin.id == id then return true end end
  return false
end

-- /panels/agents: what runs and what the user can talk to. One action per list item.
local function panel_tree(render_id)
  local workers = read(render_id, 'hub.workers').workers or {}
  local agents = read(render_id, 'hub.agents').agents or {}
  local has_messages = registered('messaging', render_id)
  local nodes, agent_items, worker_items = {}, {}, {}
  if navigation_notice then nodes[#nodes + 1] = {id = 'navigation_notice', type = 'widget', text = navigation_notice} end
  for _, agent in ipairs(agents) do
    if agent.id ~= 'user' then
      agent_items[#agent_items + 1] = {id = agent.id, label = agent.name or agent.id,
        detail = agent.joined and (agent.ready and 'Ready' or 'Connected') or 'Not connected',
        action = has_messages and action('message', 'Message', navigation) or nil}
    end
  end
  nodes[#nodes + 1] = {id = 'agents', type = 'list', label = 'Agents', items = agent_items}
  for _, worker in ipairs(workers) do
    local label = worker_label(worker)
    local item_action
    if worker.state == 'failed' then
      label, item_action = label .. ' · Dismiss', action('dismiss', 'Dismiss', {tool = 'hub.stop'})
    elseif active(worker) then
      item_action = action('manage', 'Manage…', navigation)
    elseif worker.window_id and worker.state ~= 'closed' then
      item_action = action('open', 'Open', navigation)
    end
    worker_items[#worker_items + 1] = {id = worker.spawn_id, label = label, detail = worker_detail(worker), action = item_action}
  end
  nodes[#nodes + 1] = {id = 'workers', type = 'list', label = 'Workers and accessories', items = worker_items}
  return {title = 'Agents', nodes = nodes}
end

-- /forms/worker/<spawn_id>: the menu of one active worker.
local function worker_tree(spawn_id, render_id)
  local worker
  for _, w in ipairs(read(render_id, 'hub.workers').workers or {}) do if w.spawn_id == spawn_id then worker = w end end
  if not worker then
    return {title = 'Worker', menu = true, nodes = {
      {id = 'gone', type = 'widget', text = 'This worker is no longer listed.'}, cancel_node('Close')}}
  end
  local nodes = {{id = 'detail', type = 'widget', text = worker_detail(worker)}}
  if worker.window_id and worker.state ~= 'closed' then
    nodes[#nodes + 1] = {id = 'open', type = 'widget', text = '', action = action('open', 'Open', navigation)}
  end
  if active(worker) then
    nodes[#nodes + 1] = {id = 'stop', type = 'widget', text = '', action = action('stop', 'Stop', {tool = 'hub.stop'})}
  end
  nodes[#nodes + 1] = cancel_node('Close')
  return {title = worker_label(worker), menu = true, nodes = nodes}
end

-- /forms/stop: the keyboard path, a select of the active workers.
local function stop_tree(render_id)
  local options = {}
  for _, worker in ipairs(read(render_id, 'hub.workers').workers or {}) do
    if active(worker) then options[#options + 1] = {value = worker.spawn_id, label = worker_label(worker)} end
  end
  local nodes = {}
  if #options == 0 then
    nodes[1] = {id = 'no_workers', type = 'widget', text = 'No worker or accessory runs.'}
  else
    nodes[1] = {id = 'stop', type = 'form', label = 'Stop a worker or accessory', fields = {
      {id = 'worker', label = 'Worker', kind = 'select', required = true, value = options[1].value, options = options}
    }, submit_action = action('stop', 'Stop', {tool = 'hub.stop'})}
  end
  nodes[#nodes + 1] = cancel_node()
  return {title = 'Stop Worker', nodes = nodes}
end

-- /forms/launch: one launch form per profile (registered only while no Profiles plugin is).
local function launch_tree(render_id)
  local profiles_ok, profile_result = pcall(read, render_id, 'hub.profiles')
  local profiles = profiles_ok and (profile_result.profiles or {}) or {}
  local nodes = {}
  for _, profile in ipairs(profiles) do
    local fields = {{id = 'title', label = 'Tab title (optional)', kind = 'text', required = false, value = ''}}
    for _, arg in ipairs(profile.args or {}) do
      fields[#fields + 1] = {id = 'arg/' .. arg.name, label = arg.label or arg.name,
        kind = arg.name == 'task' and 'multiline' or 'text', required = arg.required == true, value = arg.default or ''}
    end
    nodes[#nodes + 1] = {id = 'launch/' .. profile.id, type = 'form', label = 'Launch ' .. (profile.name or profile.id),
      fields = fields, submit_action = action('launch', 'Launch', {tool = 'hub.spawn'})}
  end
  if not profiles_ok then
    local reason = type(profile_result) == 'table' and profile_result.message or tostring(profile_result)
    nodes[#nodes + 1] = {id = 'profiles_error', type = 'widget', text = 'Profiles could not be loaded: ' .. (reason or 'Unknown error')}
  elseif #profiles == 0 then
    nodes[#nodes + 1] = {id = 'no_profiles', type = 'widget', text = 'No profiles are configured.'}
  end
  nodes[#nodes + 1] = cancel_node()
  return {title = 'Launch Profile', nodes = nodes}
end

-- The plugin manager (C8, docs/hub.md "Plugin manager"): Plugins… lists the entries, and each
-- entry has a menu. Every op is the view effect plugins = {op, entry, expect, input}: the hub
-- admits it as the user's op, bound to the view operation key. The entry key (managed:<id>, or
-- the source of a config entry) goes into the route as hex, so a render needs no Lua state.
-- A form that sent an op stays open and shows the newest op of its own view (origin_view).
local confirmation = {user_only = 'confirmation'} -- new code or a new entry: only the user decides
local op_tool = {tool = 'hub.plugins.op'}
local final = {done = true, failed = true, conflict = true, unknown = true}
local function hex(s) return (s:gsub('.', function(c) return string.format('%02x', c:byte()) end)) end
local function unhex(h)
  if type(h) ~= 'string' or h == '' or #h % 2 == 1 or h:find('[^0-9a-f]') then fail('invalid_input', 'Unknown plugin route') end
  return (h:gsub('..', function(x) return string.char(tonumber(x, 16)) end))
end
local function entry_key(e) return e.managed and ('managed:' .. e.managed) or e.source end
local function entry_of(list, key)
  for _, e in ipairs(list.entries or {}) do if entry_key(e) == key then return e end end
end
local function widget(id, text, a) return {id = id, type = 'widget', text = text, action = a} end
local function button(id, name, label, parity) return widget(id, '', action(name, label, parity)) end
-- digest16 is the first 16 hex digits of the SHA-256 of text (hub.digest: Rex Lua has no hash).
local function digest16(render_id, text) return read(render_id, 'hub.digest', {texts = {text}}).digests[1]:sub(1, 16) end
local function entry_detail(e)
  local d = e.state or 'unknown'
  if e.state == 'stopped' and e.desired == 'stopped' then d = 'Disabled: stays stopped until you enable it' end
  if e.tag then d = d .. ' · ' .. e.tag end
  if e.commit then d = d .. ' · ' .. e.commit:sub(1, 12) end
  -- The lock can pin another commit than the one that runs: it runs at the next explicit start.
  if (e.pinned_commit or '') ~= '' and e.pinned_commit ~= e.commit then d = d .. ' · pinned ' .. e.pinned_commit:sub(1, 12) end
  d = e.source .. ' · ' .. d
  if type(e.error) == 'string' and e.error ~= '' then d = d .. ' · ' .. e.error end
  if type(e.candidate) == 'table' then
    d = d .. (e.candidate.error and (' · New code refused: ' .. e.candidate.error) or ' · New code is ready: it runs at the next start')
  end
  return d
end
-- progress_node shows the newest op that this view sent, or that it sent none.
local function progress_node(render_id, view_id)
  local op = (read(render_id, 'hub.plugins.ops', {origin_view = view_id}).ops or {})[1]
  local text = 'No operation yet.'
  if op then
    text = 'Last operation: ' .. op.op .. ' · ' .. op.phase
    local why = op.error ~= '' and op.error or op.result
    if final[op.phase] and why and why ~= '' then text = text .. ' · ' .. why end
  end
  return widget('progress', text)
end
local function gone(title, render_id, view_id)
  return {title = title, menu = true, nodes = {widget('gone', 'This plugin entry is no longer listed.'), progress_node(render_id, view_id), cancel_node('Close')}}
end
-- settings_text is the managed entry's args (sorted key=value lines) and watch roots (one per
-- line), or nil when a form cannot show them exactly: a line break in a key or value, "=" in a key,
-- or over 65536 bytes (the hub.digest limit of one text).
local function settings_text(e)
  local keys, lines = {}, {}
  for k in pairs(e.args or {}) do keys[#keys + 1] = k end
  table.sort(keys)
  for _, k in ipairs(keys) do
    if k:find('[=\r\n]') or e.args[k]:find('[\r\n]') then return nil end
    lines[#lines + 1] = k .. '=' .. e.args[k]
  end
  for _, r in ipairs(e.watch_roots or {}) do if r:find('[\r\n]') then return nil end end
  local a, w = table.concat(lines, '\n'), table.concat(e.watch_roots or {}, '\n')
  if #a + #w + 1 > 65536 then return nil end
  return a, w
end
local function settings_id(render_id, a, w, f) return 'settings/' .. digest16(render_id, a .. '\0' .. w .. '\0' .. (f or '')) end
-- fetch_field is the choice of where a GitHub entry's files come from (docs/hub.md "Plugin sync").
local function fetch_field(value, peer)
  return {id = 'fetch', label = 'Take the plugin files from', kind = 'select', required = true, value = value, options = {
    {value = 'github', label = 'GitHub'},
    {value = 'peer', label = 'The peer hub' .. (type(peer) == 'string' and (' ' .. peer) or '') .. ' (this hub needs no GitHub access)'}}}
end
local function fetch_value(values)
  local f = values.fetch
  if f == nil or f == '' then return 'github' end
  if f ~= 'github' and f ~= 'peer' then fail('invalid_input', 'Select where the files come from') end
  return f
end
local function start_id(render_id, x) return 'start/' .. digest16(render_id, x.source .. '\0' .. (x.commit or '') .. '\0' .. (x.digest or '')) end
-- An offer (hub.plugins "offered"): a GitHub source that the plugins_from peer pins and this hub
-- does not have. offer_id binds the complete offer, a fixed field list (docs/hub.md "Plugin sync").
local function offer_of(list, source)
  for _, o in ipairs(list.offered or {}) do if o.source == source then return o end end
end
local function offer_id(render_id, o)
  local p = o.pin
  return 'offer/' .. digest16(render_id, table.concat({o.peer, o.id, o.source, p.ref, p.ref_kind, p.tag or '', p.commit, p.digest}, '\0'))
end
local function offer_pin_text(p)
  return p.ref .. ' (' .. p.ref_kind .. ((p.tag or '') ~= '' and (', release ' .. p.tag) or '') .. ')'
end
local function lines_of(text)
  local out = {}
  for line in (text .. '\n'):gmatch('(.-)\r?\n') do if line ~= '' then out[#out + 1] = line end end
  return out
end
local function version_field(render_id, source)
  local t = read(render_id, 'hub.plugins.tags', {source = source})
  local options = {{value = 'latest', label = 'Latest release' .. (t.latest and (' (' .. t.latest.tag .. ')') or '')}}
  for _, tag in ipairs(t.tags or {}) do options[#options + 1] = {value = tag.tag, label = tag.tag} end
  local state = 'Versions: ' .. (t.state or 'unknown') .. ((t.error or '') ~= '' and (' · ' .. t.error) or '')
  return {id = 'version', label = 'Version', kind = 'select', required = true, value = 'latest', options = options}, widget('versions', state, action('refresh', 'Refresh versions', navigation))
end

local function plugins_panel(render_id)
  local list = read(render_id, 'hub.plugins')
  local nodes, items = {}, {}
  for _, r in ipairs(list.refused or {}) do
    if r.managed_file then nodes[#nodes + 1] = widget('managed_file', 'The managed plugin file ' .. r.managed_file .. ' does not read, so its entries are not loaded: ' .. r.error) end
  end
  for _, e in ipairs(list.entries or {}) do
    items[#items + 1] = {id = entry_key(e), label = e.name or entry_key(e), detail = entry_detail(e), action = action('manage', 'Manage…', navigation)}
  end
  for _, k in ipairs(list.kept or {}) do
    items[#items + 1] = {id = 'kept:' .. k.name, label = k.name, detail = 'Kept record with no entry · ' .. (k.state or 'unknown') .. ((k.error or '') ~= '' and (' · ' .. k.error) or '')}
  end
  nodes[#nodes + 1] = {id = 'entries', type = 'list', label = 'Plugins', items = items}
  local offered = {}
  for _, o in ipairs(list.offered or {}) do
    offered[#offered + 1] = {id = o.source, label = o.id ~= '' and o.id or o.source, action = action('review', 'Review…', navigation),
      detail = 'Offered by ' .. o.peer .. ' · ' .. o.source .. ' · pinned by the peer at ' .. offer_pin_text(o.pin) .. ' · ' .. o.pin.commit:sub(1, 12)}
  end
  if #offered > 0 then nodes[#nodes + 1] = {id = 'offered', type = 'list', label = 'Offered by a peer hub', items = offered} end
  nodes[#nodes + 1] = button('add', 'add', 'Add plugin…', navigation)
  return {title = 'Plugins', nodes = nodes}
end

local function entry_tree(key, render_id, view_id)
  local list = read(render_id, 'hub.plugins')
  local e = entry_of(list, key)
  if not e then return gone('Plugin', render_id, view_id) end
  local nodes = {widget('detail', entry_key(e) .. '\n' .. entry_detail(e)), progress_node(render_id, view_id)}
  local github = e.source:sub(1, 7) == 'github:'
  -- Desired running: the hub runs it or tries to (waiting, crashing). Disable stops that too.
  local trying = e.desired == 'running' and e.state ~= 'refused' and e.state ~= 'retained'
  if e.state ~= 'running' and not trying then nodes[#nodes + 1] = button('enable', 'enable', 'Enable (start)', github and navigation or op_tool) end
  if e.name ~= 'lazyrex' and e.desired == 'running' then nodes[#nodes + 1] = button('disable', 'disable', 'Disable (stop)', op_tool) end
  if e.state == 'running' then nodes[#nodes + 1] = button('restart', 'restart', 'Restart', op_tool) end
  if e.managed and github and e.fetch == 'peer' then
    nodes[#nodes + 1] = widget('update', 'Its files come from the peer hub: the plugin sync updates it. To update it from GitHub, set GitHub in the settings first.')
  elseif e.managed and github then
    nodes[#nodes + 1] = button('update', 'update', 'Update…', navigation)
  end
  if e.managed then
    local a, w = settings_text(e)
    if a then
      local fields = {
        {id = 'args', label = 'Args: one key=value per line', kind = 'multiline', required = false, value = a},
        {id = 'watch_roots', label = 'Watch roots: one path per line', kind = 'multiline', required = false, value = w},
      }
      if e.fetch then fields[#fields + 1] = fetch_field(e.fetch, list.plugins_from) end
      nodes[#nodes + 1] = {id = settings_id(render_id, a, w, e.fetch), type = 'form', label = 'Settings (they apply at the next start)', fields = fields,
        submit_action = action('settings', 'Save settings', op_tool)}
    else
      nodes[#nodes + 1] = widget('settings', 'These settings cannot be edited here (a line break, "=" in a key, or over 64 KiB). Change them with the agent tool hub.plugins.op.')
    end
    if e.name ~= 'lazyrex' then nodes[#nodes + 1] = button('remove', 'remove', 'Remove…', navigation) end
  end
  nodes[#nodes + 1] = button('log', 'log', 'Log', navigation)
  for _, p in ipairs(list.plugins or {}) do
    if p.id == e.name and p.panel then nodes[#nodes + 1] = button('open', 'open', 'Open', navigation) end
  end
  nodes[#nodes + 1] = cancel_node('Close')
  return {title = e.name or key, menu = true, nodes = nodes}
end

-- offer_tree confirms the add of an offer: the form names the complete offer, and its Add binds it.
local function offer_tree(source, render_id, view_id)
  local list = read(render_id, 'hub.plugins')
  local o = offer_of(list, source)
  local nodes = {}
  if not o then
    nodes[1] = widget('gone', 'This offer is no longer listed.')
  elseif o.id == '' then
    nodes[1] = widget('no_id', o.peer .. ' has ' .. o.source .. ' in its config file, so it offers no id. Add it with Add plugin….')
  else
    local used
    for _, e in ipairs(list.entries or {}) do if e.managed == o.id then used = e end end
    if used then
      nodes[1] = widget('id_used', 'This hub already has managed:' .. o.id .. ' (' .. used.source .. '), so the offer cannot be added with its id. Remove that entry first.')
    else
      nodes[1] = {id = offer_id(render_id, o), type = 'form', label = 'Add managed:' .. o.id .. ' from ' .. o.peer .. '? Source ' .. o.source .. ', pinned by the peer at ' .. offer_pin_text(o.pin) ..
        ', commit ' .. o.pin.commit .. ' (' .. o.pin.digest .. '). This runs the plugin\'s code on this computer.', fields = {fetch_field('github', o.peer)},
        submit_action = action('add', 'Add', confirmation)}
    end
  end
  nodes[#nodes + 1] = progress_node(render_id, view_id)
  nodes[#nodes + 1] = cancel_node('Close')
  return {title = 'Add Offered Plugin', nodes = nodes}
end

local function confirm_tree(what, key, render_id, view_id)
  if what == 'offer' then return offer_tree(key, render_id, view_id) end
  local e = entry_of(read(render_id, 'hub.plugins'), key)
  if not e then return gone('Confirm', render_id, view_id) end
  local nodes = {}
  if what == 'remove' then
    nodes[1] = widget('remove', 'Remove ' .. key .. ' (' .. e.source .. ')? It stops, and its entry leaves the managed file.', action('remove', 'Remove', op_tool))
  elseif not e.expect_run then
    nodes[1] = widget('no_run', 'This entry has no code to start: ' .. ((e.candidate or {}).error or e.error or 'unknown'))
  elseif e.expect_run.commit then
    local x = e.expect_run
    nodes[1] = widget(start_id(render_id, x), 'Run ' .. x.source .. ' at commit ' .. x.commit .. ' (' .. x.digest .. ')? This runs code from GitHub on this computer.', action('start', 'Start', confirmation))
  else
    nodes[1] = widget(start_id(render_id, e.expect_run), 'Run the code in ' .. (e.dir or e.source) .. ' as it is at the start?', action('start', 'Start', confirmation))
  end
  nodes[#nodes + 1] = progress_node(render_id, view_id)
  nodes[#nodes + 1] = cancel_node('Close')
  return {title = what == 'remove' and 'Remove Plugin' or 'Start Plugin', nodes = nodes}
end

local function add_tree(source_hex, render_id, view_id)
  if not source_hex then
    return {title = 'Add Plugin', nodes = {{id = 'source', type = 'form', label = 'GitHub source', fields = {
      {id = 'source', label = 'Source: github:<owner>/<repo>[/<subdir>], with no @ref', kind = 'text', required = true, value = ''}},
      submit_action = action('next', 'Next', confirmation)}, cancel_node()}}
  end
  local source = unhex(source_hex)
  local version, versions = version_field(render_id, source .. '@latest')
  return {title = 'Add Plugin', nodes = {widget('shown_source', source), versions,
    {id = 'add', type = 'form', label = 'Add ' .. source, fields = {version,
      {id = 'id', label = 'Id (optional: the default comes from the source)', kind = 'text', required = false, value = ''}},
      submit_action = action('add', 'Add', confirmation)},
    progress_node(render_id, view_id), button('back', 'back', 'Back', navigation), cancel_node('Close')}}
end

local function update_tree(key, render_id, view_id)
  local e = entry_of(read(render_id, 'hub.plugins'), key)
  if not e then return gone('Update', render_id, view_id) end
  local version, versions = version_field(render_id, e.source)
  return {title = 'Update ' .. (e.name or key), nodes = {widget('shown_source', e.source), versions,
    {id = 'update', type = 'form', label = 'Update to', fields = {version}, submit_action = action('update', 'Update', confirmation)},
    progress_node(render_id, view_id), cancel_node('Close')}}
end

-- log_tree: the plugin pane's last 60 lines (escape sequences removed) and its hub.log lines.
local function log_tree(key, render_id)
  local r = read(render_id, 'hub.plugins.log', {entry = key})
  local text = 'Log: ' .. (r.state or 'unknown') .. ((r.error or '') ~= '' and (' · ' .. r.error) or '')
  local nodes = {widget('state', text, action('refresh', 'Refresh', navigation))}
  if type(r.block) == 'table' then
    local pane = (r.block.text or ''):gsub('\27%[[%d;?]*[@-~]', ''):gsub('\27%][^\7\27]*\7', ''):gsub('\27.', ''):gsub('[%z\1-\8\11-\31\127]', '')
    local all, from = lines_of(pane), 1
    if #all > 60 then from = #all - 59 end
    nodes[#nodes + 1] = widget('pane', 'Pane output:\n' .. table.concat(all, '\n', from) .. ((r.block.error or '') ~= '' and ('\n' .. r.block.error) or ''))
  end
  if type(r.hub_log) == 'table' then
    local lines = r.hub_log.lines or {}
    nodes[#nodes + 1] = widget('hub_log', 'Hub log:\n' .. (#lines > 0 and table.concat(lines, '\n') or (r.hub_log.reason or r.hub_log.error or 'No lines.')))
  end
  return {title = 'Plugin Log', nodes = nodes}
end

local function manager_tree(route, render_id, view_id)
  if route == '/panels/plugins' then return plugins_panel(render_id) end
  if route == '/forms/plugins/add' then return add_tree(nil, render_id, view_id) end
  local kind, h = route:match('^/[a-z]+/plugins/([a-z/]+)/(%x+)$')
  local key = unhex(h)
  if kind == 'entry' then return entry_tree(key, render_id, view_id) end
  if kind == 'add' then return add_tree(h, render_id, view_id) end
  if kind == 'update' then return update_tree(key, render_id, view_id) end
  if kind == 'log' then return log_tree(key, render_id) end
  return confirm_tree(kind:match('^confirm/(.+)$'), key, render_id, view_id)
end

-- manager_action answers a manager action, or nil for another route. It reads the hub again: an
-- op names the state that the user acts on now (expect), not the state of an older render.
local function manager_action(args, input)
  local route = args.route
  if not (route:find('^/panels/plugins') or route:find('^/forms/plugins/')) then return nil end
  local values = type(input.values) == 'table' and input.values or {}
  if route == '/panels/plugins' then
    if args.node_id == 'add' then return {form = '/forms/plugins/add'} end
    if args.node_id == 'offered' then
      local o = offer_of(hub('hub.plugins'), input.item_id)
      if not o then fail('not_found', 'The selected offer is no longer available. Refresh the view.') end
      return {form = '/forms/plugins/confirm/offer/' .. hex(o.source)}
    end
    local e = entry_of(hub('hub.plugins'), input.item_id)
    if args.node_id ~= 'entries' or not e then fail('not_found', 'The selected item is no longer available. Refresh the view.') end
    return {form = '/forms/plugins/entry/' .. hex(entry_key(e))}
  end
  if route == '/forms/plugins/add' then
    local source = values.source
    if type(source) ~= 'string' or not source:match('^github:[^@%s]+$') then fail('invalid_input', 'Give a GitHub source github:<owner>/<repo>[/<subdir>], with no @ref') end
    return {form = '/forms/plugins/add/' .. hex(source)}
  end
  local kind, h = route:match('^/[a-z]+/plugins/([a-z/]+)/(%x+)$')
  local key = unhex(h)
  if args.action == 'refresh' then
    if kind == 'log' then hub('hub.plugins.log', {entry = key, refresh = true}) else
      local e = kind == 'update' and entry_of(hub('hub.plugins'), key)
      hub('hub.plugins.tags', {source = e and e.source or (key .. '@latest'), refresh = true})
    end
    return {}
  end
  if kind == 'add' then
    if args.action == 'back' then return {form = '/forms/plugins/add'} end
    if type(values.version) ~= 'string' or values.version == '' then fail('invalid_input', 'Select a version') end
    local id = type(values.id) == 'string' and values.id ~= '' and values.id or nil
    return {plugins = {op = 'add', input = {source = key .. '@' .. values.version, id = id}}}
  end
  if kind == 'confirm/offer' then
    -- The press reads the offer again: another or a withdrawn offer sends no op. The hub compares
    -- the complete offer again at admission (target).
    local o = offer_of(hub('hub.plugins'), key)
    if not o or o.id == '' or offer_id(nil, o) ~= args.node_id then fail('conflict', 'The offered code changed since it was shown. Read the new text, then add again.') end
    return {plugins = {op = 'add', input = {source = o.source, id = o.id, fetch = fetch_value(values)}, target = o}}
  end
  local list = hub('hub.plugins')
  local e = entry_of(list, key)
  if not e then fail('not_found', 'This plugin entry is no longer listed.') end
  local x = e.expect_run
  if kind == 'update' then
    if type(values.version) ~= 'string' or values.version == '' then fail('invalid_input', 'Select a version') end
    return {plugins = {op = 'update', entry = key, input = {to = values.version}}}
  end
  if kind == 'confirm/remove' then return {plugins = {op = 'remove', entry = key}} end
  if kind == 'confirm/start' then
    if not x or start_id(nil, x) ~= args.node_id then fail('conflict', 'The code to start changed since it was shown. Read the new text, then start again.') end
    return {plugins = {op = 'start', entry = key, expect = x}}
  end
  local a = args.action
  if a == 'enable' and e.source:sub(1, 7) == 'github:' then return {form = '/forms/plugins/confirm/start/' .. h} end
  if a == 'enable' then
    if not x then fail('invalid_input', 'This entry has no code to start: ' .. ((e.candidate or {}).error or e.error or 'unknown')) end
    return {plugins = {op = 'start', entry = key, expect = x}}
  end
  if a == 'disable' then return {plugins = {op = 'stop', entry = key}} end
  if a == 'restart' then return {plugins = {op = 'restart', entry = key}} end
  if a == 'update' then return {form = '/forms/plugins/update/' .. h} end
  if a == 'remove' then return {form = '/forms/plugins/confirm/remove/' .. h} end
  if a == 'log' then return {panel = {route = '/panels/plugins/log/' .. h}} end
  if a == 'open' then
    for _, p in ipairs(list.plugins or {}) do if p.id == e.name and p.panel then return {panel = {plugin = p.id, command = p.panel}} end end
    fail('not_found', 'The plugin has no panel now.')
  end
  if a == 'settings' then
    local cur_a, cur_w = settings_text(e)
    if not cur_a or settings_id(nil, cur_a, cur_w, e.fetch) ~= args.node_id then fail('conflict', 'The settings changed since they were shown. Read the new values, then save again.') end
    local new_args, roots = {}, lines_of(type(values.watch_roots) == 'string' and values.watch_roots or '')
    for n, line in ipairs(lines_of(type(values.args) == 'string' and values.args or '')) do
      local k, v = line:match('^([^=]+)=(.*)$')
      if not k then fail('invalid_input', 'args: line ' .. n .. ' needs key=value') end
      if new_args[k] then fail('invalid_input', 'args: the key ' .. k .. ' is given twice') end
      new_args[k] = v
    end
    -- An empty list is left out: Rex Lua encodes an empty table as {}, and the hub takes a missing
    -- watch_roots as an empty one.
    local cur_roots = #(e.watch_roots or {}) > 0 and e.watch_roots or nil
    -- fetch: only a GitHub entry has it (hub.plugins); nil keeps the value at the hub.
    local fetch = e.fetch and fetch_value(values) or nil
    return {plugins = {op = 'settings', entry = key, input = {args = new_args, watch_roots = #roots > 0 and roots or nil, fetch = fetch},
      expect = {args = e.args or {}, watch_roots = cur_roots, fetch = e.fetch}}}
  end
  fail('invalid_input', 'Unknown plugin manager action')
end

local function render(route, render_id, view_id)
  if route:find('^/panels/plugins') or route:find('^/forms/plugins/') then return manager_tree(route, render_id, view_id) end
  if route == '/panels/agents' then return panel_tree(render_id) end
  if route == '/forms/stop' then return stop_tree(render_id) end
  if route == '/forms/launch' then return launch_tree(render_id) end
  return worker_tree(route:match('^/forms/worker/(.+)$'), render_id)
end

local function stop(spawn_id, key)
  -- The hub owns stop retries, including a retry after the worker disappears.
  return hub('hub.stop', {spawn_id = spawn_id, key = key, ['for'] = 'user'})
end
local function closing(result)
  result.close_form = true
  return result
end
local function perform(args)
  local input = type(args.input) == 'table' and args.input or {}
  local managed = manager_action(args, input)
  if managed then return managed end
  if args.node_id == 'agents' and args.action == 'message' then
    local agent = find(hub('hub.agents').agents, 'id', input.item_id)
    if agent.id == 'user' then fail('invalid_input', 'Select an agent') end
    return {navigate = {plugin = 'messaging', tab = 'thread/' .. agent.id}}
  end
  if args.node_id == 'workers' and (args.action == 'open' or args.action == 'manage') then
    local worker = find(hub('hub.workers').workers, 'spawn_id', input.item_id)
    if args.action == 'manage' then return {form = '/forms/worker/' .. worker.spawn_id} end
    return hub('hub.navigate.worker', {spawn_id = worker.spawn_id})
  end
  local menu_id = args.route:match('^/forms/worker/(.+)$')
  if menu_id and args.node_id == 'open' and args.action == 'open' then
    local worker = find(hub('hub.workers').workers, 'spawn_id', menu_id)
    return closing(hub('hub.navigate.worker', {spawn_id = worker.spawn_id}))
  end
  if type(args.operation_key) ~= 'string' or args.operation_key == '' then fail('invalid_input', 'An operation key is required') end
  if args.node_id == 'workers' and args.action == 'dismiss' then
    if type(input.item_id) ~= 'string' or input.item_id == '' then fail('invalid_input', 'Select a worker') end
    return stop(input.item_id, args.operation_key)
  end
  if menu_id and args.node_id == 'stop' and args.action == 'stop' then
    return closing(stop(menu_id, args.operation_key))
  end
  local values = type(input.values) == 'table' and input.values or {}
  if args.route == '/forms/launch' and type(args.node_id) == 'string' and string.sub(args.node_id, 1, 7) == 'launch/' and args.action == 'launch' then
    local profile = string.sub(args.node_id, 8)
    if profile == '' then fail('invalid_input', 'Select a profile') end
    if type(values.title) ~= 'string' then fail('invalid_input', 'The title must be text') end
    local request = {profile = profile, key = args.operation_key, ['for'] = 'user'}
    local named_args = {}
    for name, value in pairs(values) do
      if string.sub(name, 1, 4) == 'arg/' then
        if type(value) ~= 'string' then fail('invalid_input', 'Launch inputs must be text') end
        named_args[string.sub(name, 5)] = value
      end
    end
    if next(named_args) then request.args = named_args end
    if values.title ~= '' then request.title = values.title end
    local result = hub('hub.spawn', request)
    if result.status == 'open' and type(result.spawn_id) == 'string' then
      local shown = pcall(hub, 'hub.navigate.worker', {spawn_id = result.spawn_id})
      set_notice(not shown and 'The worker started. Select the worker in Agents… to show its tab.' or nil)
      return closing(result)
    end
    set_notice(nil)
    return result -- a failed launch keeps the form, with its values
  end
  if args.route == '/forms/stop' and args.node_id == 'stop' and args.action == 'stop' then
    if type(values.worker) ~= 'string' or values.worker == '' then fail('invalid_input', 'Select a worker') end
    return closing(stop(values.worker, args.operation_key))
  end
  fail('invalid_input', 'Unknown home action')
end

-- launch_offered: another registered plugin has a palette command whose action is hub.spawn, as
-- Profiles' Start Agent… and Start Accessory… are. The capability decides, not a plugin name.
local function launch_offered()
  for _, plugin in ipairs(hub('hub.plugins').plugins or {}) do
    if plugin.id ~= 'lazyrex' then
      for _, c in ipairs(plugin.commands or {}) do if c.action == 'hub.spawn' then return true end end
    end
  end
  return false
end
-- The palette entries. "Launch Profile…" only while no other plugin offers a launch (decision Q1 A,
-- 2026-10-22; the hub lead: gate on the capability).
local base_commands = {
  {id = 'agents', title = 'Agents…', route = '/panels/agents', surface = 'panel', landing = true, user_only = 'navigation'},
  {id = 'stop', title = 'Stop Worker…', route = '/forms/stop', action = 'hub.stop'},
  {id = 'plugins', title = 'Plugins…', route = '/panels/plugins', surface = 'panel', user_only = 'navigation'},
}
local function commands()
  local list = {}
  for i, c in ipairs(base_commands) do list[i] = c end
  if not launch_offered() then
    list[#list + 1] = {id = 'launch', title = 'Launch Profile…', route = '/forms/launch', action = 'hub.spawn'}
  end
  return list
end
-- sync registers again only when the command set changed. A registration makes the hub send
-- hub.changed (plugins) again, which then finds no change: no loop. Each call reads the current
-- plugin list, so the last of a burst of changes wins.
local registered_ids
local function sync()
  local list = commands()
  local ids = {}
  for _, c in ipairs(list) do ids[#ids + 1] = c.id end
  ids = table.concat(ids, ',')
  if ids == registered_ids then return end
  hub('hub.register', {plugin = 'lazyrex', instance_id = instance_id, exports = {}, commands = list})
  registered_ids = ids
end

local routes = {['/panels/agents'] = true, ['/forms/stop'] = true, ['/forms/launch'] = true,
  ['/panels/plugins'] = true, ['/forms/plugins/add'] = true}
local manager_routes = {'^/forms/plugins/entry/%x+$', '^/forms/plugins/add/%x+$', '^/forms/plugins/update/%x+$',
  '^/forms/plugins/confirm/start/%x+$', '^/forms/plugins/confirm/remove/%x+$', '^/forms/plugins/confirm/offer/%x+$', '^/panels/plugins/log/%x+$'}
local function known_route(route)
  if type(route) ~= 'string' then return false end
  if routes[route] or route:match('^/forms/worker/.+$') then return true end
  for _, p in ipairs(manager_routes) do if route:match(p) then return true end end
  return false
end
local handlers = {
  ['view.render'] = function(args) return {tree = render(args.route, args.render, args.view_id)} end,
  ['view.action'] = perform,
  ['view.seen'] = function(args) return {} end,
  ['hub.changed'] = function(args) sync(); return {} end,
}
local actions = {}
for name in pairs(handlers) do actions[#actions + 1] = {name = name, kind = 'client', title = name,
  args = {type = 'object', additionalProperties = true}} end
rex.call('client.keymap', {actions = actions})
-- The first registration names the base commands, so the hub knows the plugin; sync then adds
-- Launch Profile… when no other plugin offers a launch.
hub('hub.register', {plugin = 'lazyrex', instance_id = instance_id, exports = {}, commands = base_commands})
registered_ids = 'agents,stop,plugins'
sync()
hub('hub.ui.tabs', {tabs = {}})
rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if not ev.requester or ev.requester.client_id ~= hub_id then fail('refused', 'Only the hub calls home actions') end
    local handler = handlers[ev.action]
    if not handler then fail('not_supported', 'Unknown home action') end
    local args = ev.args or {}
    if ev.action ~= 'hub.changed' and (not known_route(args.route) or args.user ~= 'user') then fail('refused', 'Unknown home route or user') end
    return handler(args)
  end)
  if ok then rex.call('client.reply', {call_id = ev.call_id, result = result})
  else rex.call('client.reply', {call_id = ev.call_id, error = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}}) end
end)
rex.call('client.set_label', {label = 'lazyrex-plugin:lazyrex'})

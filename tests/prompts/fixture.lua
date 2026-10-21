-- A plugin with the prompts grant that tests agent prompts (docs/hub.md, "Agent prompts and acting
-- as the user") through the real hub. It applies hub.prompt pushes and hub.prompts snapshots with
-- the plugin's apply rule of docs/hub.md ("Prompt pushes"), so it is also a reference for a real
-- plugin. The runner calls:
--   pf.hub {action, args}: a hub call as this plugin; returns {result} or {error = {code, message}}
--   pf.state: the applied state {generation, prompts, applied, floor, pushes}
--   pf.snapshot: calls hub.prompts and applies it
--   pf.inject {kind = 'push' | 'snapshot', msg}: applies a message as if the hub had sent it
--   pf.fail {n}: the next n pushes are applied, and then their replies fail (a lost reply)
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')

-- State lives in table fields: a closure write to a captured local can be lost after a caught
-- error (rex-protocol.md, "Rex Lua runtime").
local S = {generation = nil, prompts = {}, applied = {}, floor = 0, pushes = 0, fail = 0}

local function call_hub(action, args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if err then
    local text = type(err) == 'table' and (err.message or '') or tostring(err)
    local code = type(err) == 'table' and err.code or text:match('code=([%w_%-%.]+)%)%s*$')
    return {error = {code = code, message = text}}
  end
  return {result = res and res.result}
end

local function effective(agent)
  return math.max(S.applied[agent] or 0, S.floor)
end

-- apply_push: rule 1. A push of another hub generation is ignored (rule 3).
local function apply_push(m)
  if m.hub_generation ~= S.generation then return 'ignored' end
  local r = tonumber(m.rev)
  if r <= effective(m.agent) then return 'ignored' end
  S.prompts[m.agent] = m.prompt -- nil (JSON null) removes the prompt
  S.applied[m.agent] = r
  return 'applied'
end

-- apply_snapshot: rule 2.
local function apply_snapshot(snap)
  if snap.hub_generation ~= S.generation then return 'ignored' end
  local R = tonumber(snap.rev)
  local listed, agents = {}, {}
  for _, p in ipairs(snap.prompts or {}) do
    listed[p.agent] = p
    agents[p.agent] = true
  end
  for a in pairs(S.prompts) do agents[a] = true end
  for a in pairs(S.applied) do agents[a] = true end
  for a in pairs(agents) do
    if effective(a) < R then
      S.prompts[a] = listed[a]
      S.applied[a] = R
    end
  end
  S.floor = math.max(S.floor, R)
  return 'applied'
end

local obj = {type = 'object'}
rex.call('client.keymap', {actions = {
  {name = 'view.render', kind = 'client', title = 'Render', args = obj},
  {name = 'hub.changed', kind = 'client', title = 'Hub change notice', args = obj},
  {name = 'hub.prompt', kind = 'client', title = 'The current prompt state of one agent', args = obj},
  {name = 'pf.hub', kind = 'client', title = 'Call a hub action as the plugin', args = obj},
  {name = 'pf.state', kind = 'client', title = 'The applied prompt state', args = obj},
  {name = 'pf.snapshot', kind = 'client', title = 'Apply hub.prompts', args = obj},
  {name = 'pf.inject', kind = 'client', title = 'Apply a push or a snapshot', args = obj},
  {name = 'pf.fail', kind = 'client', title = 'Fail the replies of the next n pushes', args = obj},
}})

rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    local args = ev.args or {}
    if ev.action == 'view.render' then
      return {tree = {title = 'Prompt fixture', nodes = {{id = 'text', type = 'widget', text = 'Prompt fixture'}}}}
    elseif ev.action == 'hub.changed' then
      return {}
    elseif ev.action == 'hub.prompt' then
      S.pushes = S.pushes + 1
      local outcome = apply_push(args)
      if S.fail > 0 then
        S.fail = S.fail - 1
        error({code = 'failed', message = 'fixture: the reply is lost after the apply'})
      end
      return {outcome = outcome}
    elseif ev.action == 'pf.hub' then
      return call_hub(args.action, args.args)
    elseif ev.action == 'pf.state' then
      return {generation = S.generation, prompts = S.prompts, applied = S.applied, floor = S.floor, pushes = S.pushes}
    elseif ev.action == 'pf.snapshot' then
      local r = call_hub('hub.prompts', {})
      if r.error then return r end
      return {outcome = apply_snapshot(r.result), snapshot = r.result}
    elseif ev.action == 'pf.inject' then
      if args.kind == 'push' then return {outcome = apply_push(args.msg)} end
      return {outcome = apply_snapshot(args.msg)}
    elseif ev.action == 'pf.fail' then
      S.fail = args.n or 1
      return {}
    end
    error({code = 'invalid_input', message = 'unknown action ' .. tostring(ev.action)})
  end)
  if ok then
    rex.call('client.reply', {call_id = ev.call_id, result = result})
  else
    local e = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}
    rex.call('client.reply', {call_id = ev.call_id, error = e})
  end
end)

-- Rule 3: the generation comes only from this plugin's own accepted registration.
local reg = call_hub('hub.register', {plugin = 'promptfix', instance_id = instance_id, exports = {}})
assert(not reg.error, 'hub.register failed')
S.generation = reg.result.generation
call_hub('hub.ui.tabs', {tabs = {{key = 'home', title = 'Prompt fixture', route = '/home'}}})
rex.call('client.set_label', {label = 'lazyrex-plugin:promptfix'})

-- A plugin for the in-place replacement lab (inplace_lab_test.go). The command a opens the form
-- /forms/inplace/a. Each form shows LAB-FORM-<letter> and one action, Next, which opens the next
-- form from the form (the effect {form = route}): a -> b -> c. The form c has Finish, which ends
-- it ({close_form = true}). The command p opens the panel /panels/inplace (LAB-PANEL), whose
-- action Open opens the form a; a form that replaced the panel returns to it when it ends.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')

local function hub(action, args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if err or not res then
    error({code = 'hub_failed', message = type(err) == 'table' and (err.message or '') or tostring(err)})
  end
  return res.result
end

local next_of = {a = 'b', b = 'c'}

local function render(args)
  if args.route == '/panels/inplace' then
    return {tree = {title = 'In place panel', nodes = {
      {id = 'notice', type = 'widget', text = 'LAB-PANEL'},
      {id = 'open', type = 'widget', action = {name = 'open', label = 'Open a', enabled = true}},
    }}}
  end
  local letter = args.route:match('^/forms/inplace/(%a)$')
  if not letter then
    return {tree = {title = 'Unknown', nodes = {{id = 'unknown', type = 'error', text = 'no route ' .. tostring(args.route)}}}}
  end
  local nodes = {{id = 'notice', type = 'widget', text = 'LAB-FORM-' .. letter:upper()}}
  if next_of[letter] then
    nodes[#nodes + 1] = {id = 'next', type = 'widget', action = {name = 'next', label = 'Next ' .. letter, enabled = true}}
  end
  if letter == 'c' then
    nodes[#nodes + 1] = {id = 'finish', type = 'widget', action = {name = 'finish', label = 'Finish', enabled = true}}
  end
  return {tree = {title = 'In place ' .. letter, nodes = nodes}}
end

local obj = {type = 'object'}
rex.call('client.keymap', {actions = {
  {name = 'view.render', kind = 'client', title = 'Render', args = obj},
  {name = 'view.action', kind = 'client', title = 'Action', args = obj},
}})
rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if ev.requester.client_id ~= hub_id then error({code = 'refused', message = 'Only the hub may call ' .. ev.action}) end
    if ev.action == 'view.render' then return render(ev.args) end
    if ev.action == 'view.action' then
      if ev.args.route == '/panels/inplace' and ev.args.node_id == 'open' then return {form = '/forms/inplace/a'} end
      local letter = (ev.args.route or ''):match('^/forms/inplace/(%a)$')
      if ev.args.node_id == 'next' and letter and next_of[letter] then
        return {form = '/forms/inplace/' .. next_of[letter]}
      end
      if ev.args.node_id == 'finish' and letter == 'c' then return {close_form = true} end
      return {}
    end
    if ev.action:sub(1, 5) == 'view.' then return {} end
    error({code = 'unknown_action', message = ev.action})
  end)
  if ok then
    rex.call('client.reply', {call_id = ev.call_id, result = result})
  else
    local e = type(result) == 'table' and result or {code = 'internal', message = tostring(result)}
    rex.call('client.reply', {call_id = ev.call_id, error = e})
  end
end)

hub('hub.register', {plugin = 'inplace', instance_id = instance_id,
  commands = {{id = 'a', title = 'In place lab', route = '/forms/inplace/a', user_only = 'a lab view'},
    {id = 'p', title = 'In place panel', route = '/panels/inplace', surface = 'panel', user_only = 'a lab view'}}})
rex.call('client.set_label', {label = 'lazyrex-plugin:inplace'})

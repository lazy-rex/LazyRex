-- A plugin for TestReplyPartsHoldLab (chunk_lab_test.go). Its form /forms/parts renders about
-- 150 KiB in reply parts (docs/hub.md "Reply parts"). The SQL row hold(gen, release) drives it: the
-- first and the last node show gen; when release is 0, the render stops before its last part, sets
-- held(n) to 1 and waits until a writer sets release to 1 (at most 8 s: the hub's render timeout is 10 s). The render reads hold with
-- its token (a write renders the view again) and reads release and held without it.
local hub_id = assert(rex.args.hub_client_id, 'hub_client_id is required')
local instance_id = assert(rex.args.instance_id, 'instance_id is required')

local function hub(action, args)
  local res, err = rex.call('client.perform', {client_id = hub_id, action = action, args = args or {}, timeout_ms = 10000})
  if err or not res then
    error({code = 'hub_failed', message = type(err) == 'table' and (err.message or '') or tostring(err)})
  end
  return res.result
end

local function query(sql, render)
  return hub('hub.sql.query', {sql = sql, params = {}, render = render}).rows[1]
end

local function wait_ms(ms) -- rex.sleep does not return in every plugin context; a CPU wait does
  local t = os.clock() + ms / 1000
  while os.clock() < t do end
end

local seq = 0
local function send_parts(value, before_last)
  local text = rex.json.encode(value)
  seq = seq + 1
  local id = 'hp-' .. instance_id:gsub('[^%w._:-]', '') .. '-' .. seq
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
    if k == #slices then before_last() end
    hub('hub.reply.part', {id = id, index = k - 1, count = #slices, data = s})
  end
  return {lazyrex_parts = {id = id, count = #slices, bytes = #text}}
end

local function render(args)
  local row = query('SELECT gen, release FROM hold', args.render) or {}
  local gen, release = row[1] or 0, row[2]
  local text = string.rep('aé"\\<&日', 60)
  local nodes = {{id = 'gen', type = 'widget', text = 'gen ' .. gen}}
  for k = 1, 240 do nodes[#nodes + 1] = {id = 'n' .. k, type = 'widget', text = text} end
  nodes[#nodes + 1] = {id = 'last', type = 'widget', text = 'end ' .. gen}
  return send_parts({tree = {title = 'Parts', nodes = nodes}}, function()
    if release ~= 0 then return end
    hub('hub.sql.exec', {sql = 'UPDATE held SET n = 1'})
    for _ = 1, 160 do
      local r = query('SELECT release FROM hold')
      if r and r[1] == 1 then return end
      wait_ms(50)
    end
  end)
end

rex.on('client_perform_requested', function(ctx, ev)
  local ok, result = pcall(function()
    if ev.requester.client_id ~= hub_id then error({code = 'refused', message = 'Only the hub may call ' .. ev.action}) end
    if ev.action == 'view.render' then return render(ev.args) end
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

hub('hub.register', {plugin = 'holdp', instance_id = instance_id,
  commands = {{id = 'parts', title = 'Parts', route = '/forms/parts', user_only = 'a lab view'}}})
hub('hub.sql.exec', {sql = 'CREATE TABLE IF NOT EXISTS hold (gen INTEGER, release INTEGER)'})
hub('hub.sql.exec', {sql = 'CREATE TABLE IF NOT EXISTS held (n INTEGER)'})
rex.call('client.set_label', {label = 'lazyrex-plugin:holdp'})

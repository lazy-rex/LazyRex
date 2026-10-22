-- a:print writes known text to the agent's pane, as an agent CLI writes output, so a test can find
-- it on the screen: block.method with the short name format (formats text, html or vt; text is the
-- default; observed on Rex 0.0.1792158932-g88fd41).

local function identity(id)
  for _, i in ipairs(H.hub('hub.identity.list', {}).identities or {}) do
    if i.identity == id then return i end
  end
end

local function screen(session, block)
  local r, err = rex.call('block.method', {session_id = session, block_id = block, method = 'format', args = {format = 'text'}})
  if err ~= nil then error('format: ' .. tostring(err), 0) end
  return r.content or ''
end

test('a:print puts its text on the agent screen', function()
  local a = agent('printer', {join = false})
  a:report({kind = 'claude', conversation_id = 'print-1'})
  local i
  wait('the hub knows the pane', function()
    i = identity('claude:print-1')
    return i and i.block_id ~= nil
  end)
  a:print('lazyrex print check 7f3a\r\n')
  wait('the text is on the screen', function() return screen(a.session, i.block_id):find('lazyrex print check 7f3a', 1, true) ~= nil end)
end)

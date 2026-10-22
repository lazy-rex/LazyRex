-- Reply parts through the real SDK (docs/hub.md "Reply parts", docs/plugin-sdk.md "Reply size").
local app = require('lazyrex').plugin('sp')

app.db.migrate {'CREATE TABLE saves (x)', 'CREATE TABLE big_ticks (x)', 'CREATE TABLE small_ticks (x)'}

-- sp.save writes one row and answers about kb KiB, so a keyed answer is saved through reply_parts.
app.action('sp.save', {kb = 'integer'}, function(ctx, args)
  app.db.exec('INSERT INTO saves VALUES (1)')
  return {kb = args.kb, data = string.rep('s', args.kb * 1024)}
end, {description = 'Write one row and answer kb KiB.'})

app.action('sp.count', {}, function()
  return {n = app.db.first('SELECT count(*) AS n FROM saves').n}
end, {description = 'Count the rows.'})

-- Two panels for the timing lab (view/chunk_lab_test.go): about 150 KiB (in parts) and 40 KiB
-- (inline). Each reads its own table, so a write renders one panel only.
local function filler(kb, t)
  local n = app.db.first('SELECT count(*) AS n FROM ' .. t).n
  local text = string.rep('aé"\\<&日', 60)
  local per = #rex.json.encode(app.ui.text('n1000', text)) + 1
  local nodes = {app.ui.text('ticks', 'ticks ' .. n)}
  for k = 1, math.floor(kb * 1024 / per) do nodes[#nodes + 1] = app.ui.text('n' .. k, text) end
  return nodes
end
app.panel('Big', function() return filler(150, 'big_ticks') end, {landing = false})
app.panel('Small', function() return filler(40, 'small_ticks') end, {landing = false})

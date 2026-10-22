-- SQL read tracking through the real SDK: two opted-in panels and one default panel.
local app = require('lazyrex').plugin('rr')

app.db.migrate {
  'CREATE TABLE a (x)',
  'CREATE TABLE b (x)',
  'CREATE TABLE other (x)',
}

app.action('rr.add', {t = {type = 'string', enum = {'a', 'b', 'other'}}}, function(ctx, args)
  app.db.exec('INSERT INTO ' .. args.t .. ' VALUES (1)')
  return {}
end, {description = 'Add a row to a table.'})

local function count(t)
  local n = app.db.first('SELECT count(*) AS n FROM ' .. t).n
  return {app.ui.text('n', tostring(n))}
end
app.panel('Alpha', function() return count('a') end, {refresh = 'reads'})
app.panel('Beta', function() return count('b') end, {refresh = 'reads', landing = false})
app.panel('Gamma', function() return count('a') end, {landing = false})

-- A caught query failure must prevent the SDK from opting this render in.
local function caught(raw)
  app.db.first('SELECT count(*) AS n FROM a')
  local ok = pcall(function()
    if raw then
      app.hub('hub.sql.query', {sql = 'SELECT count(*) FROM b WHERE ? IS NOT NULL', params = {'review-raw-fail'}})
    else
      app.db.query('SELECT count(*) FROM b WHERE ? IS NOT NULL', 'review-db-fail')
    end
  end)
  return {app.ui.text('caught', ok and 'complete' or 'caught')}
end
app.panel('Caught DB', function() return caught(false) end, {refresh = 'reads', landing = false})
app.panel('Caught Raw', function() return caught(true) end, {refresh = 'reads', landing = false})

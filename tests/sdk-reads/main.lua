-- SQL read tracking through the real SDK: two tracked panels (the default) and one with refresh = "all".
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
app.panel('Alpha', function() return count('a') end)
app.panel('Beta', function() return count('b') end, {landing = false})
app.panel('Gamma', function() return count('a') end, {refresh = 'all', landing = false})

-- A caught query failure makes the SDK reply refresh = "all" for this render.
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
app.panel('Caught DB', function() return caught(false) end, {landing = false})
app.panel('Caught Raw', function() return caught(true) end, {landing = false})

-- Covered hub reads in a render: the SDK sends the render ID through the hub's declared argument
-- schemas, and a render that read only covered reads stays tracked.
app.panel('Hub Reads', function()
  app.hub('hub.workers')
  app.hub('hub.profiles')
  return {app.ui.text('reads', 'ok')}
end, {landing = false})

-- hub.plugins is a covered read with the change name "plugins" (C8). A reload sends that change.
app.action('rr.reload', {}, function(ctx, args)
  app.hub('hub.plugins.reload')
  return {}
end, {description = 'Read the managed plugin file again.'})
app.panel('Plugin Reads', function()
  app.hub('hub.plugins')
  return {app.ui.text('plugins', 'ok')}
end, {landing = false})
-- hub.digest is a covered pure read: it keeps the render tracked and adds no change name.
app.panel('Digest Reads', function()
  local r = app.hub('hub.digest', {texts = {'a'}})
  return {app.ui.text('digest', r.digests[1])}
end, {landing = false})

-- app.db.migrate {base = N} through the real hub (docs/plugin-sdk.md, "A base step").
local app = require('lazyrex').plugin('sm')

app.db.migrate {base = 3, 'CREATE TABLE a (x)', 'CREATE TABLE b (x)'}

app.action('sm.tables', {}, function()
  local names = {}
  app.db.each("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name", function(r) names[#names + 1] = r.name end)
  return {version = app.db.first('PRAGMA user_version').user_version, tables = names}
end, {description = 'The schema version and the tables.'})

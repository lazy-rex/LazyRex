-- Reply parts through the real SDK (docs/hub.md "Reply parts", docs/plugin-sdk.md "Reply size").
local app = require('lazyrex').plugin('sp')

app.db.migrate {'CREATE TABLE saves (x)'}

-- sp.save writes one row and answers about kb KiB, so a keyed answer is saved through reply_parts.
app.action('sp.save', {kb = 'integer'}, function(ctx, args)
  app.db.exec('INSERT INTO saves VALUES (1)')
  return {kb = args.kb, data = string.rep('s', args.kb * 1024)}
end, {description = 'Write one row and answer kb KiB.'})

app.action('sp.count', {}, function()
  return {n = app.db.first('SELECT count(*) AS n FROM saves').n}
end, {description = 'Count the rows.'})

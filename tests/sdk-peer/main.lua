-- Federation through the real SDK (app.peer, docs/plugin-sdk.md "Federation") on one lab hub. The
-- peer "away" has no server: hub.peer.send queues for it, and a test pulls and acks as "away".
local app = require('lazyrex').plugin('speer')

app.db.migrate {'CREATE TABLE got (from_hub TEXT, key TEXT, len INTEGER, head TEXT, text TEXT)'}

local peer
peer = app.peer{receive = function(msg)
  local t = msg.body.t
  app.db.exec('INSERT INTO got (from_hub, key, len, head, text) VALUES (?, ?, ?, ?, ?)', msg.from_hub, msg.key, #t, t:sub(1, 16), t)
end}

local ANY = {type = 'object', additionalProperties = true}

-- speer.send queues {t = text} to hub with key k.
app.action('speer.send', ANY, function(ctx, a) return peer.send(a.hub, a.k, {t = a.text}) end,
  {description = 'Queue one envelope.'})

-- speer.send_wait commits the send in its own transaction, then waits s seconds in the handler and
-- reads the row's state: the drain is armed (rex.after 0), but Rex runs no timer while a handler runs.
app.action('speer.send_wait', ANY, function(ctx, a)
  app.db.transaction(function() peer.send(a.hub, a.k, {t = a.text}) end)
  rex.sleep(a.s)
  return {state = app.db.first('SELECT state FROM sdk_peer_outbox WHERE key = ?', a.k).state}
end, {tx = false, description = 'Send, then wait in the handler.'})

app.action('speer.status', {}, function() return peer.status() end, {store = false, description = 'peer.status().'})

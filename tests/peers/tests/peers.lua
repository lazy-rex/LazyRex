-- Peer hubs on one lab hub (docs/hub.md, "Peer hubs"): hub.test.peer.deliver gives an envelope to
-- the plugin through the real delivery path, and hub.peer.send queues for a peer that cannot be
-- reached. The manifest sets test.hub_name "lab" and one peer "away" with no server.

local function pf(action, args)
  local st = H.hub('hub.test.status', {})
  return H.perform(st.plugins.peerfix.client_id, action, args or {})
end

test('an accepted envelope reaches peer.receive unchanged', function()
  local body = '{"v":1,"text":"hello"}'
  local r = H.peer_deliver({from_hub = 'away', key = 'k1', body = body})
  H.assert_eq(r.reply.stored, 1, 'the plugin reply')
  local got = pf('pf.got').got
  H.assert_eq(got[1], {from_hub = 'away', key = 'k1', body = body}, 'the envelope')
end)

test('a refusal comes back as the refused result', function()
  local r = H.peer_deliver({from_hub = 'away', key = 'refuse', body = 'x'})
  H.assert_eq(r.reply.refused, 'unknown_recipient', 'refused code')
  H.assert_eq(r.reply.message, 'no such agent', 'refused message')
end)

test('a raised error is not processed', function()
  H.expect_error(function() H.peer_deliver({from_hub = 'away', key = 'raise', body = 'x'}) end, 'not_processed')
end)

test('hub.peer.send queues for a peer that cannot be reached', function()
  local s = pf('pf.send', {hub = 'away', key = 'out1', body = 'to away'})
  H.assert_true(type(s.seq) == 'number', 'a seq')
  local st = H.peer_status()
  H.assert_eq(st.hub_name, 'lab', 'hub_name')
  H.assert_eq(st.peers[1].name, 'away', 'peer name')
  H.assert_eq(st.peers[1].outbox, 1, 'outbox')
end)

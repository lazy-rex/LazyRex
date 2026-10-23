# Peer hubs on one lab hub

`lazyrex-hub test tests/peers` runs the receive side and the send side of peer hubs (docs/hub.md,
"Peer hubs") on one lab hub:

- `hub.test.peer.deliver` (harness `H.peer_deliver`) gives an envelope to the fixture plugin
  through the hub's delivery path for a pulled envelope: an accepted envelope, a refusal
  (`{refused = code, message}` comes back as the reply), and a raised error (`not_processed`).
- `hub.peer.send` to the peer `away` (manifest `test.peers`, a server that does not exist) queues
  the envelope; `hub.peer.status` (harness `H.peer_status`) shows it in the outbox.

Not covered here: delivery between two real hubs. `TestPeerHubsLab` (HARNESS_LAB=1) covers that.

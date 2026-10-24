# sdk-peer

The SDK federation helper (`app.peer`, docs/plugin-sdk.md "Federation") against the real hub and
SQL on one lab hub. The peer `away` has no server, so `hub.peer.send` queues for it, and a test
pulls and acks as `away` with `hub.peer.pull`. Run: `lazyrex-hub test tests/sdk-peer`.

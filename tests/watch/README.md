# hub.watch tests

Lua tests of the hub's file watches (docs/hub.md, "File watches") on the real hub and a real
fsnotify watcher, in a lab. Run:

```sh
lazyrex-hub test tests/watch
```

The fixture plugin `watchfix` (`fixture.lua`) may watch `LAB/watched` (manifest
`test.watch_roots`). The runner calls `wf.hub` to make a `hub.watch.*` call as the plugin, and
`wf.nudges` to read the nudges that the plugin got.

## Coverage

| Case | Test |
| --- | --- |
| Refusals: outside the roots, missing path, `recursive`, a bad key | watch.lua |
| created, changed, removed; the baseline file hidden until it changes | watch.lua |
| ensure is idempotent; another spec is `watch_conflict` | watch.lua |
| An atomic save gives one row; hidden temp files give none | watch.lua |
| Many writes give one row | watch.lua |
| glob | watch.lua |
| A nudge after a commit; a dropped nudge is repeated with no later change | watch.lua |
| remove with the current `watch_id` only; a late remove, read or ack with the old one; new seqs | watch.lua |
| An ack past the watermark is refused; a stale ack | watch.lua |
| A directory over the entry limit is refused at ensure | watch.lua |
| Growth past the entry limit closes the watch (`too_large`) | watch.lua |
| The directory replaced with a symlink outside the roots (`not_granted`) | watch.lua |
| A regular file replaced with a symlink reads as removed | watch.lua |
| A slow writer during initialization; an unrelated hub call answers | watch.lua |
| `emit_existing` | watch.lua |
| A hub crash: the watch stays; a file that came and went leaves nothing | lifecycle.lua |
| A crash during initialization resumes with the same baseline | lifecycle.lua |
| A plugin restart: the watch stays, rows wait, the new instance gets a nudge | lifecycle.lua |
| A directory renamed away: no removal; back in place, the real removal | lifecycle.lua |
| A timer that fires before the directory event is handled commits nothing | `hub/cmd/hub/watchsvc_test.go` |

Not covered by a lab test:

- `backlog_full` at 20000 rows. A unit test covers it with a lower limit (`watch_test.go`).
- The 5-minute timer itself. `TestWatchPeriodicReopensAfterShrink` (`watchsvc_test.go`) calls the
  scan directly: growth past the limit, no reopen above 90%, a reopen at 899 entries.
- A backend error on one watch while another watch stays active. No test injects a backend
  error.
- The hub descriptor budget in a lab. `TestWatchBudgetCountsBackendDescriptors` covers the count.
- macOS sleep.

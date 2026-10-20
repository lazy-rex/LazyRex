# Home plugin tests

Lua tests of the home plugin (`plugins/lazyrex/lazyrex.lua`) on the real hub (docs/testing.md).
Run:

```sh
lazyrex-hub test plugins/lazyrex
```

The manifest loads a messaging stub (`stubs/messaging/`: a landing panel and lazy `thread/<id>`
tabs) and the harness fixture (`tests/harness`, a plugin with a tab). Profiles come from
`profiles.json`. The
`task` profile writes its two args to `LAB/home/task-args`. The `status` profile reports an
OSC 7501 status record.

## Coverage map: tests/home.mjs

`tests/home.mjs` ran the plugin against a mock hub. The Lua tests replace it, and the file is
removed (git history has it). Each row names its lines and where the Lua tests cover the behavior
on the real hub.

| home.mjs | Behavior | Lua test |
| --- | --- | --- |
| 50, 52 | Registration and one tab `home` at `/` | render.lua "declares one tab" |
| 53-54 | `view.render` returns `{tree}` | every `View:render` (the hub reads `tree`) |
| 56-58 | Title; other plugins only; agents without the user | render.lua "the tree lists" |
| 59 | Worker detail from the status record (`idle · Ready`) | render.lua "a worker detail shows its program status record" (the `status` profile reports OSC 7501) |
| 60 | Stop options: active workers only | render.lua "workers" |
| 61 | Failed detail `failed · <error>` | render.lua "workers" (a spawn-create refuse fault) |
| 62-65 | Launch form fields, defaults, multiline task | render.lua "the tree lists" |
| 66-71 | A profile error keeps navigation and worker controls | render.lua "a profile error" |
| 73-106 | App mode and Exit LazyRex | Obsolete: decision "One LazyRex session; app mode retired". The plugin has no app mode node. |
| 107-109 | Plugin open is a navigate effect; no `hub.ui.navigate` call | navigation.lua "a plugin with a tab" (the result is the effect) |
| — | Plugin open with a landing panel | navigation.lua "a landing panel" (new) |
| 110 | Message an agent | navigation.lua "message an agent" |
| 111-112 | Open a worker | navigation.lua "open a worker" |
| 113-115 | Launch with a title, then show the worker | launch.lua "user path"; agent path: "agent path" |
| 116-119 | Named inputs reach the spawn | launch.lua "named inputs" (both paths; the worker receives the args) |
| 120-122 | Named inputs survive a retry after profile removal | launch.lua "user path: the hub retry after a profile removal" (a lose-reply fault, then a gate holds the retry while the test removes the profile); agent path: "agent path: a retry after the profile was removed" |
| 123 | Launch inputs must be text | launch.lua "launch inputs must be text" |
| 125-126 | A launch retry reaches the hub after profile removal | launch.lua (same tests as 120-122) |
| 127-129 | Dismiss a failed worker | stop.lua "a failed launch is dismissed" (both paths) |
| 130-131 | Stop form | stop.lua "user path", "agent path" |
| — | An agent can stop a worker that the user started | stop.lua "an agent can stop" (new; decision "Agents may act on anything", 2026-10-19) |
| 132 | An operation key is required | Not reachable through the hub: the hub always sends a key. The check stays in the plugin. |
| 133 | Only the hub may call | stop.lua "only the hub may call" |
| 134-135 | Unknown route or user | Not reachable through the hub: the hub sends route `/` and user `user` for the home tab. The check stays in the plugin. |
| 136 | A missing item | navigation.lua "an item that is gone" (plugins, agents, workers) |
| 137-142 | A navigation failure does not fail the launch; notice | navigation.lua "a launch whose navigation fails" (an action fault on `hub.navigate.worker`) |
| 143-146 | `no_profiles` and no stop form | render.lua "without profile files": the hub always has the builtin shell profile, so `no_profiles` does not show on the real hub. No stop form without active workers: render.lua "the tree lists". |
| — | A definite hub error ends a launch; a transient one is retried | errors.lua (new; the hub() wrapper fix) |

Renderer rows: home.mjs called the plugin directly and pressed no keys, so it has no renderer rows.

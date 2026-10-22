# Home plugin tests

Lua tests of the home plugin (`plugins/lazyrex/lazyrex.lua`) on the real hub (docs/testing.md).
Run:

```sh
lazyrex-hub test plugins/lazyrex
```

The home plugin has no tab. Its palette entries are "Agents…" (the panel `/panels/agents`, the
landing), "Stop Worker…" (`/forms/stop`) and, only while no other plugin offers a launch,
"Launch Profile…" (`/forms/launch`). The gate is a capability: no other registered plugin has a
palette command whose action is `hub.spawn`. An active worker's "Manage…" opens the menu form
`/forms/worker/<spawn_id>`.

The manifest loads three plugins:

- a messaging stub (`stubs/messaging/`: a landing panel and lazy `thread/<id>` tabs);
- a launcher stub (`stubs/launcher/`): it registers a palette command whose action is `hub.spawn`,
  as Profiles' Start Agent… does. A test stops and starts it with `hub.admin.stop` and
  `hub.admin.start`. `launch.lua` and `errors.lua` stop it first, so the launch form is registered.
  In `capability.lua` it registers with no command (`test.files` args);
- the harness fixture (`tests/harness`, a plugin with a tab).

Profiles come from `profiles.json`. The `task` profile writes its two args to
`LAB/home/task-args`. The `status` profile reports an OSC 7501 status record. The parity fixture
(`parity_fixture.lua`) opens "Launch Profile…" and a worker menu.

## Coverage

| Behavior | Test |
| --- | --- |
| No tabs; the palette entries; Launch Profile… only without a launch command elsewhere | render.lua "declares no tabs"; commands.lua; capability.lua (a registered plugin without the command) |
| A burst of launcher changes: one final command set, the latest; no registration loop | commands.lua "a burst of changes" |
| Panel: agents without the user, Message only with Messaging | render.lua "the panel lists the agents"; navigation.lua "Messaging absent" |
| Panel: Manage… for an active worker, Dismiss for a failed one | render.lua "workers" |
| Worker detail from the status record (`idle · Ready`) | render.lua "a worker detail" |
| A closed worker leaves the list and the stop options | render.lua "a worker whose pane closed" |
| Worker menu: Open, Stop, Close; menu = true | render.lua "a worker menu" |
| Message: a failed open keeps the panel; a retry opens the thread | navigation.lua "Messaging fails the open" |
| Menu Open returns to the panel; a refused Open keeps the menu and the hub code | navigation.lua "worker menu: Open", "a refused Open" |
| Menu Escape (the renderer ends) returns to the panel | navigation.lua "Escape" |
| A worker that disappears: the menu shows it, without Stop | navigation.lua "a worker that disappears" |
| A missing item fails with a refresh hint | navigation.lua "an item that is gone" |
| Stop Worker… form (closes after the stop); menu Stop (returns to the panel) | stop.lua "user path" (both) |
| Same-key replay; another request under the key is refused; a gone worker stops again | stop.lua "a duplicate Stop" |
| Agent stop and replay; Dismiss on both paths; the form needs a worker | stop.lua |
| Only the hub may call | stop.lua "only the hub may call" |
| Launch form: fields per profile; a success closes it, a failure keeps it | launch.lua |
| Named inputs, retry after a profile removal, a changed request under a used key | launch.lua |
| A navigation failure after a launch: the panel shows a notice | launch.lua "a launch whose navigation fails" |
| Profile errors and the builtin shell | launch.lua "a profile error", "without profile files" |
| A definite hub error ends a launch; a transient one is retried | errors.lua |

Not reachable through the hub: a missing operation key, and an unknown route or user. The hub
always sends a key, and it sends only declared routes. The checks stay in the plugin.

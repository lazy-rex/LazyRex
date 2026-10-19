# Handoff: harness team (2026-10-19)

The harness pair stops here. This note lists the open work in LazyRex. Messaging has its own
HANDOFF.md on its `harness` branch.

## State of main

- LazyRex origin/main is 3fa19d2. It contains the work of this team: testview re-attach,
  hub-stop-any, askrelease (never-attached panel claim), and C2, the hub reply guard
  (`hub/cmd/hub/replyguard.go`, docs/hub.md "Reply size").
- The live hub runs release clones. Pushing to main does not deploy. A release needs the user's
  approval, through the coordinator.

## Branches

- `harness-handoff` (this branch, on 3fa19d2): this note, plus the lab fact for plugin sources
  (rex-protocol.md "Rex Lua runtime", probe `lab/probes/cache-dir-siblings.lua`). Pushed.
- `readsize` (local only, not pushed): `store_budget.go` (budgeted `hub.store.get`,
  `hub.store.chunk`). Superseded: the coordinator and the reviewer dropped it after the
  measurement that a plugin receives replies of more than 64 KiB. It is kept only for reference;
  it can be deleted.
- `replyguard`, `askrelease`, `stopany`, `reattach`, `label`, `home`: merged into main.

## Open task: plugin sources (design not written)

Task from the coordinator (plugin-sources-task-1). Design first, to the coordinator and the
reviewer; no code until the reviewer approves.

User decisions (2026-10-19):
1. A plugin entry names a source: a local directory, or a GitHub repository with a ref. Branch
   refs (`@branch`) are allowed. A lock pins every GitHub source to a commit until an explicit
   update.
2. A manifest lists the plugins it depends on. The hub refuses to start a plugin whose dependency
   is missing, with a clear error.

Coordinator proposal (to correct in the design):
- Config: `{"source": "<dir>"}` or `{"source": "github:<owner>/<repo>@<ref>"}`, plus optional args.
  The manifest (`lazyrex.json`: name, main, events, test) owns name, main and events; path and
  events leave the config entry, with no compatibility path.
- GitHub: fetch with the git CLI (the user's own credentials). Cache each commit read-only under
  `<state>/plugins/<owner>/<repo>/<sha>/`. A lock file records ref and commit. Hub start reads only
  the lock and the cache, with no network. An explicit command (for example
  `lazyrex-hub plugin update [name]`) fetches and rewrites the lock.
- Local sources run in place (hot reload keeps working).
- `lazyrex-hub test` uses the same loader (for example Worktrees naming Profiles exports).

The design must answer: the dependency field format and version rule; an update that fails half
way; a dirty or missing cache entry (refused or fetched again); the error text of each refusal.

Reviewer conditions (all open): define config, manifest, test loader and generated lab configs
together; the existing `requires` entries in `testcmd.go` (name, path, events); where `notice`
moves (it is a behavior field, not redundant with events); the home plugin; duplicate names,
missing and transitive dependencies, cycles, startup order and readiness; relative paths and
their base (config or manifest location); exact source and ref identity in the lock; first install
versus update; offline start with a missing or dirty cache; partial failures; concurrent start and
update; atomic publication of cache plus lock; callers and artifacts that must change together;
changes in other repositories that need another owner.

Lab check (observed, Rex 0.0.1792158932-g88fd41, lab): `rex do` runs a main file from a read-only
directory `<base>/plugins/acme/widget/<40 hex>/`; `debug.getinfo` gives the absolute path;
`dofile` of a sibling, `require` (the script's directory is first in `package.path`) and reads
work; a write in the directory is refused. The reviewer has not checked this result.

Facts for the design (source): the hub launches a plugin with
`rex -S <server> --autostart=false do <path> -s <session> hub_client_id=… instance_id=…`
(`plugins.go` spawn), or `--all plugin_session=…` for events "all". Config entries are parsed in
`main.go` (`Config.Plugins`: name, path, notice, events, args). Plugins find siblings from
`debug.getinfo(1, 'S').source` (for example `Tunnels/tunnels.lua:25`).

## Known limits from this team's work

- Reply guard: on the agent face, a forwarded plugin result cannot reach the disconnect size; the
  test proves that the guard engages there, not that it prevents a disconnect.
- Hub test replies to a Lua runner are limited to 49152 bytes by the test agent; a runner cannot
  observe a plugin reply near 64 KiB.
- The alias migration in Messaging writes every renamed record in one batch (not bounded).

## Next step

Write the plugin sources design (above) and send it to the coordinator and the reviewer.

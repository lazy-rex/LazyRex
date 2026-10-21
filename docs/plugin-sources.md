# Plugin sources

Status: **design approved** (v3, 2026-10-20, the hub reviewer). This document joins design v1 with
the corrections of v2 and v3. Each section says what is built. Approval of the design is not
approval of code: each commit gets a source review and independent tests.

Built on main:

- Step 1: the manifest rules and the dependency graph (`hub/cmd/hub/sources.go`). These functions
  read and check. They start nothing.
- Step 2a: both manifest parsers accept `notice`.
- Step 2b: the bounded manifest read (`readManifestBytes`, 64 KiB), for both loaders.
- Step 2c, part 1: the harness reads `test.plugins` (`requires` is refused), with the hub's manifest
  and graph rules. The home plugin is `plugins/lazyrex/`.
- Step 2c, part 2 (`pluginconfig.go`): config source entries (section 1), resolved once at hub
  start. A GitHub source is refused as "not installed" until step 3. The manifest owns `events`
  and `notice`; a `notify` op without a notice fails with `no_notice` (section 10). Hub start
  spawns in `startOrder`. `hub.plugins` reports `source` and `dir` for each running plugin, and a
  `refused` list. The harness lab config has local source entries only.

Step 2d part 1:

- One start path, `startPlugin` (`plugins.go`), for hub start recovery, `hub.admin.start`, a crash
  respawn and a restart after a session loss. Its order is the one of section 7, "Lifecycle".
- Each start attempt resolves every entry again (`resolveEntries`). A name binds at the first
  resolution that gives it without a conflict. A duplicate name refuses every entry with it (step
  1), also when one entry resolved earlier: that owner keeps its name and a running instance; the
  other entry binds nothing. Two entries with one name at hub start: neither binds.
- Kept records (reviewer, ownership model approved 2026-10-20). The plugins row records the
  source of the entry that bound its name (column `source`). At hub start, each row with a recorded
  block or an open spawn intent gives a record, also when no entry gives its name now. An entry
  adopts that record when it gives the same name from the same source; a row from before the
  source was recorded matches by name only (migration rule). A record that no entry adopts is
  kept: it never starts or respawns, it gets no notices, and `hub.register` from its instance
  fails (code `retained`). Its state is retained (the child lives) or refused. `hub.admin.stop`
  resolves its intents first (an unresolved one keeps the record: code `unresolved_intent`), then
  retires its block and drops the record. `hub.admin.start` on it is refused.
- Each record loads its plugins row when the hub makes it, before an action can see it. The admin
  actions load the row again under the record's lifecycle lock. Recovery skips a record that
  `hub.admin.start` already started in this hub process.
- The ownership guard: an entry whose source a kept record holds under another name, or whose
  name a kept record holds from another source, is refused ("stop X first") and binds nothing.
  Its name still counts in the duplicate check. After the stop of X, `hub.admin.start {source}`
  starts the entry.
- The states `refused` and `retained`. A refused start retires nothing. A recorded instance that
  still lives makes the record `retained`; `hub.register` from it fails with code `retained`.
- `run`: the plugins row saves it at each spawn (column `run`). Notices use `run.notice`. Watch
  roots come from `run` while an instance is recorded, else from the config entry.
- `hub.admin.start {"source": "<exact source>"}`.
- `hub.admin.start` no longer abandons open spawn intents. An unresolved intent fails the start with
  code `unresolved_intent`. The text names the instance and the session: a `client.reply` error has
  no other fields (rex-protocol.md, "Client RPC").
- `hub.plugins` adds `entries`: one item for each config entry, with `entry`, `source`, `name`,
  `state`, `dir` (of the run), `error` (state refused or retained) and `"candidate": {"error"}`
  when the last resolution refused it. `plugins` and `refused` keep their shape. `kept` lists each
  kept record: `name`, `source`, `state`, `error`, `dir`.

On the branch `plugins/step2d2` (in review), step 2d part 2:

- The state `waiting` (`startOrWait`, `plugins.go`). After the start check and the retire, a
  plugin whose dependency has no accepted `hub.register` for its current instance waits. A
  dependency counts as registered when it has a config entry, is not retained, has a client from
  `hub.register` and a live child. An unresolved spawn intent gives no client, so the dependent
  waits. Waiting makes no spawn intent and changes no recorded ownership. The event
  `plugin_waiting {plugin, waiting}` comes when the names change.
- The wake rule (`waitLoop`): an accepted `hub.register`, a `hub.admin.start` or `hub.admin.stop`
  of any plugin, and a refusal wake the loop. It runs the start path again for each waiting plugin,
  in config order. A wake during a scan makes one more scan. A stop of a waiting plugin ends the
  wait. A stop, a refusal and a spawn clear the names.
- Hub start: a dependency spawns first; its dependent waits and spawns after the dependency
  registers.
- `hub.admin.start` of a plugin that must wait gives `{"desired": "running", "state": "waiting",
  "waiting": [names]}`. `hub.plugins` entries in state `waiting` add `waiting`.

Step 2e part 1 (managed entries, section 13) is on main (51d27c4).

Step 3 Part A is on main (08dbb89): the lock (`pluginlock.go`), the cache and `lockedDir`
(`plugincache.go`), refs and fetch (`pluginfetch.go`), and `lazyrex-hub plugin sync|update|status`
(`plugincmd.go`), with subdir sources and managed entries. Not built: step 3 Part B (the hub
side: `resolveEntries` for a GitHub source, `run.commit`, the lab test).

## Terms

- **Source**: where a plugin comes from. A local directory, or a GitHub repository at a ref.
- **Manifest**: `lazyrex.json` at the root of a source. It owns the plugin's name and behavior.
- **Lock**: a file that pins each GitHub source to one commit.
- **Cache**: the directory tree of fetched GitHub sources, under `<state>/plugins/`.
- **Entry**: one item of the config field `plugins`.
- **Instance**: one running plugin process (one block), with its instance ID.

## 1. Config entry

```json
{"source": "<dir>", "args": {}, "watch_roots": []}
{"source": "github:<owner>/<repo>@<ref>", "args": {}}
{"source": "github:<owner>/<repo>/<subdir>@<ref>", "args": {}}
```

- `args`: extra `key=value` arguments of `rex do` (`rex.args` in the plugin). Keys match
  `[a-z0-9_]+`; the hub's own keys (`hub_client_id`, `instance_id`, `plugin_session`) are refused.
- `watch_roots`: the directories whose subtrees the plugin may watch (`hub.watch`, docs/hub.md
  "File watches"). Absolute, clean paths. Without it, every `hub.watch.ensure` is refused. Only
  the user's config sets it: the manifest has no such field.
- A source that starts with `github:` is a GitHub source. Any other source is a local directory.
- A local path is absolute or relative to the directory of the config file. `~` is refused.
- `<owner>` matches `[A-Za-z0-9-]+`. `<repo>` matches `[A-Za-z0-9._-]+` and is not `.` or `..`.
- `<subdir>` is optional (the coordinator, 2026-10-21): the plugin directory inside the repository, in
  the form of a Go module path, for example `github:lazy-rex/LazyRex/plugins/lazyrex@main`. It is one or
  more segments separated by `/`. Each segment matches `[A-Za-z0-9._-]+` and is not `.` or `..`.
  No segment is empty, and there is no leading or trailing `/`. Owner, repo and subdir never
  contain `@`, so the first `@` ends the path and the ref is the rest. (A ref can contain `#`, so a
  `@ref#subdir` form would be ambiguous.) Text: `source <s>: subdir <d> must be a relative path of
  names (A-Z, a-z, 0-9, ., _, -)`.
- `<ref>` is required. See section 5 for its forms.
- The fields `name`, `path`, `events` and `notice` are refused in an entry. There is no
  compatibility path. Example text: `plugin entry 2: "path" is removed: use "source" (a directory
  with lazyrex.json, or github:owner/repo@ref)`.
- Config field `lock`: the lock file, relative to the config directory. Default: the config file
  name with a final `.json` replaced by `.lock.json` (`hub.json` gives `hub.lock.json`). A name
  without `.json` gets `.lock.json` appended. The file must exist only when the config has a
  GitHub source.

Hub start fails only for a malformed config:

- bad JSON, an unknown field or a removed field;
- a bad source string;
- two entries with the same source string: `entries 2 and 4 have the same source <source>`.

Every other problem refuses that entry only (section 7).

## 2. Manifest

Fields: `name`, `main`, `events`, `notice`, `depends`, `test`. The hub refuses unknown fields.
The file is one JSON object; data after the object is refused. (Built: step 1.)

- `name`: `[a-z0-9_-]+`, required.
- `main`: required. It is relative to the manifest directory, has no `..` component and is not
  absolute.
  - `Lstat(main)` must be a regular file. A symlink main is refused, also when it points inside the
    source.
  - A parent directory can be a symlink (for example a release clone path).
    `EvalSymlinks(main)` must be inside `EvalSymlinks(source dir)`.
- `events`: `session` (default) or `all`.
- `notice`: an optional string. It is behavior, so it belongs to the plugin. See section 10.
- `depends`: an array of plugin names, for example `["profiles"]`. A name appears once. There is
  no version rule: the lock pins exact commits. An object form can be added later.
- `test.plugins`: replaces `requires`. It is `[{"source": "<dir>"}]`, local directories only,
  relative to the manifest directory, each with its own manifest. The harness reads it; the hub
  does not. A stub can satisfy a dependency by its manifest name.

`test.plugins` is built (step 2c). `requires` is an unknown field, so it is refused.

## 3. Lock

```json
{"version": 1, "plugins": {"<exact source string>": {
  "name": "", "owner": "", "repo": "", "subdir": "", "ref": "", "ref_kind": "branch|tag|commit",
  "commit": "<40 hex>", "digest": "sha256:<hex>", "dir": "github/<owner>/<repo>/<commit>.<token>"}}}
```

- The key is the source string exactly as the config writes it. A changed source string (for
  example `@main` to `@v2`) is a new key: `sync` installs it. It is not an update of the old key.
- `dir` is relative to `<state>/plugins`.
- `subdir` is the subdir of the source, or "" without one. A version 1 lock entry without the
  field decodes as "": the version stays 1. The same repository at two subdirs gives two keys and
  two cache entries.

**Agreement check.** The hub checks a lock entry before it uses it:

- the key equals the config source;
- `owner`, `repo`, `subdir` and `ref` equal the parsed source;
- `ref_kind` matches the ref form;
- `commit` is 40 lowercase hex, and equals the ref when the ref is a commit;
- `dir` is `<owner>/<repo>/<commit>.<token>` under `github/`;
- the manifest name in the cache entry equals the lock `name`;
- the digest matches.

A mismatch refuses the entry: `the lock entry for <source> does not match: <field>; run
lazyrex-hub plugin sync`.

**Repair.** The key and the commit are the pin. Every other field is derived from them.

- A commit is **trusted** when it is 40 lowercase hex and, for a commit ref, equal to the ref.
- `sync` keeps a trusted commit and rebuilds every other field. It fetches that exact commit,
  publishes a new dir, computes the digest, and reads the name from the fetched manifest.
- An untrusted commit is malformed, not equal to a commit ref, not a commit, or not available.
  `sync` refuses that entry and the lock stays unchanged: `the locked commit for <source> cannot be
  trusted: <reason>; run lazyrex-hub plugin update -config <cfg> <source>`.
- The hub start text for an agreement error names `sync` when the commit is trusted, and the
  `update` text when it is not.

## 4. Cache

- An entry path is `<state>/plugins/github/<owner>/<repo>/<commit>.<token>/`, where `<token>` is
  16 random hex. The lock entry records the path.
- The entry is the whole commit tree, also for a source with a subdir. The digest covers the whole
  entry. The plugin directory is `<entry>/<subdir>` (the entry itself without a subdir). The main
  file rule (main inside the plugin directory) applies to that directory. A plugin can read other
  files of the commit at run time: the entry never changes.
- Published permissions: files 0444, executable files 0555, directories 0555.
- **A published entry never moves.** No command renames, deletes or writes a published entry.
  - A repair publishes a new entry and points the lock at it.
  - A running instance keeps its directory.
  - A changed entry stays on disk. `plugin status` lists each entry that the lock does not
    reference as "unreferenced (no recorded digest)": without a lock entry there is no digest to
    compare, and a running instance or another config on the same state can still use it.
- **Publication** (observed on macOS 27.0.1, APFS; Linux: not tested). A rename of a 0555
  directory into another parent fails with EACCES, because the move must update the directory's
  `..`. A rename in the same parent works. So the command extracts the tree into a stage beside
  the final path, `github/<owner>/<repo>/.stage-<token>/`, sets every published mode (the stage
  root last), and renames the stage to `<commit>.<token>` in the same parent. Nothing writes the
  entry after the rename. A failure before the rename removes the stage. Names that start with
  `.` are stages, not entries.
- There is no prune. A running plugin can `dofile` a sibling file later (Tunnels does), so the hub
  must not delete an entry that is in use.
- **Digest**: sha256 over the lines `<path>\0<x|->\0<size>\0<sha256 hex of content>\n`, sorted by
  path bytes. `x` is mode 100755. The command computes it from the archive, before it changes the
  permissions. At each plugin start the hub computes it again; there `x` is "any execute bit
  set".
- Hub start never runs git and never uses the network. It reads the config, the lock and the cache.

## 5. Refs and fetch

The hub uses the git CLI for `plugin sync` and `plugin update` only, never at start (dependency
exception approved by the coordinator). Git runs with `GIT_TERMINAL_PROMPT=0`. It uses the user's
credential helper and `url.insteadOf` (for SSH).

**Ref forms**: `<name>`, `refs/heads/<name>`, `refs/tags/<name>`, or 40 lowercase hex. A short hex
is a name, not a commit. `git check-ref-format` validates `refs/heads/<name>` and
`refs/tags/<name>`. Error: `github:o/r@<ref>: <ref> is not a valid ref`.

**Resolve**: `git ls-remote <url> refs/heads/<n> refs/tags/<n> refs/tags/<n>^{}`.

- The command keeps only lines whose name equals one of these three. A suffix match
  (`refs/heads/team/<n>`) is dropped.
- A name that is both a branch and a tag is refused: `github:lazy-rex/Messaging@foo: ref foo is
  both a branch and a tag: write refs/heads/foo or refs/tags/foo`.
- An annotated tag resolves to its `^{}` commit.
- A 40-hex ref is not resolved.
- No line matches: `github:o/r@<ref>: ref <ref> is not found`.

**Fetch** into a bare repository in `<state>/plugins/.tmp/<random>/` (`git init -q --bare`, then
`git fetch -q --depth 1`). The tree is extracted into the stage of section 4, "Publication":

1. Fetch a branch or tag by its exact ref. Fetch a commit (a 40-hex ref, or a sync of a locked
   commit) by its ID.
2. Read `git rev-parse --verify FETCH_HEAD^{commit}` and compare it with the resolved commit. Raw
   `FETCH_HEAD` is never compared. A difference: `ref <n> moved during the fetch; run the command
   again`. There is no retry loop.
3. `git cat-file -t` of the commit must be `commit`.
4. Read the tree with `git ls-tree -r -z <commit>`. Mode 100644 is a file; 100755 is an executable
   file. Mode 120000 is refused (`plugin trees with symlinks are not supported: <path>`). Mode
   160000 is refused (`submodules are not supported: <path>`).
5. `git archive --format=tar <commit>`. The hub extracts it with `archive/tar`. The tar entries
   must match `ls-tree` exactly: the same paths, no absolute path, no `..`. Each file's content
   must hash to its `ls-tree` blob ID (`blob <size>\0<content>`, SHA-1), so the published tree is
   the commit's tree. Text: `the archive of <c> does not match its tree: <path>: <reason>`.
6. With a subdir: it must be a directory of the tree with a `lazyrex.json`. Else:
   `github:<o>/<r>/<d>@<ref>: <d> is not a plugin directory at commit <c>; the lock is unchanged`.
   Then read and check the manifest of the plugin directory, compute the digest, set the
   permissions.
7. Publish with one rename of the stage to the new, unique entry path, in the same parent.

An unavailable locked commit: `github:o/r@<ref>: commit <c> is not available: <git error>; the lock
is unchanged`.

Observed by the implementer only (2026-10-20, git 2.54.0, public lazy-rex/LazyRex): `git fetch
--depth 1 <url> <sha>` of a commit that is not a branch tip worked. An unknown ID failed with
`upload-pack: not our ref`. Not independently verified. Not observed: a private repository, and a
commit that is no longer reachable (after a force push). Unit tests use a local bare repository
(`file://`) and say that they do not prove GitHub behavior.

**For plugin authors: what a GitHub source can contain.** The fetch publishes the commit's tree
exactly, or nothing:

- Regular files and executable files only. A symlink or a submodule anywhere in the repository
  refuses the source, also outside the subdir.
- `.gitattributes` `export-ignore` and `export-subst` refuse the source: they make the archive
  differ from the tree (a missing file, or changed content). Remove these attributes from the
  plugin repository.
- With a subdir, `<subdir>/lazyrex.json` must be in the commit. `main` must be inside the subdir.
  The whole commit is in the cache entry, so a plugin can read files outside its subdir at run time.
- The cache entry is read-only (files 0444 or 0555). A plugin writes its data elsewhere, for
  example in the hub store.

## 6. Commands

The commands are CLI only. A hub action must answer fast, because the hub has one request queue.

- `lazyrex-hub plugin sync -config <cfg>`: first install and repair.
  - A GitHub source without a lock entry: resolve the ref, fetch, add the entry.
  - An entry whose cache is missing or changed: fetch the locked commit again (section 3,
    "Repair").
  - It never moves an existing locked commit.
  - It drops lock entries whose source is no longer in the config.
- `lazyrex-hub plugin update -config <cfg> [name|source ...]`: resolve the refs again (all, or the
  named entries), fetch, and rewrite the lock.
- `lazyrex-hub plugin status -config <cfg>`: the source, ref, locked commit and cache state (ok,
  missing, changed) of each entry, and the changed entries that no lock references.

**All or nothing.** A command writes the lock only after every fetch and check passes (temp file,
fsync, rename). A failure leaves the old lock. Entries that the command published stay on disk;
nothing references them. So a failed command never changes a directory that a lock references.

**Graph check.** Before it writes the lock, the command checks the new dependency graph. Example
refusal: `plugin update: worktrees at <c> depends on profiles, which is not configured; the lock is
unchanged`.

**Concurrency.** A command takes two exclusive flocks, in this order:

1. `<state>/plugins/.lock`: all commands on one state, for any config or lock file.
2. `<lockfile>.lck`.

A second command fails at once: `another plugin command runs (pid N)`. No hub code in this design
takes a flock. (The plugin manager design adds a hub writer that takes both flocks; then N can be
the hub's pid.) A rename replaces the lock, and cache entries are immutable, so a hub start reads
the old lock or the new lock, never a mix.

## 7. Start, states and dependencies

**Entry identity.** Each entry has an identity: its index in the config and its exact source
string. Its name is known only from a local manifest, or from a lock entry that passed the
agreement check.

- An entry without a known name is reported as `{"entry": 3, "source": "...", "name": null,
  "state": "refused", "error": "..."}`. The hub keeps such entries in a list beside the name map,
  never under an invented name.
- Duplicate names: every entry with that name is refused: `entries 2 and 4 have the same name
  messaging`. (Built: step 1.) An entry that resolves later and finds its name in use is refused
  the same way.

**The graph.** Each start attempt rebuilds the whole graph. A start attempt is a hub start,
`hub.admin.start`, a crash respawn, or a restart after a session loss. The attempt re-reads every
local manifest and the lock, and verifies the cache entries. Then it applies the refusal rules to
the plugin to start (built: step 1):

- a missing dependency: `plugin worktrees: dependency profiles is not configured`;
- a refused dependency, transitively: `plugin worktrees: dependency profiles is refused: <its
  error>`;
- a cycle refuses each member: `plugin a: dependency cycle a -> b -> a`.

**States** in `hub.plugins`:

- `running`, `stopped`;
- `waiting`, with the names: the static check passed, and a dependency has no accepted
  `hub.register` for its current instance. (The existing register check: instance ID and pane
  ancestry.)
- `refused`, with the error: the static check failed. A dependent of a statically refused
  dependency is refused, not waiting.
- `retained`: see "Run and candidate".

**Wake rule.** The hub checks a waiting plugin again when any plugin registers, when
`hub.admin.start` or `hub.admin.stop` runs for any plugin, and when a plugin is refused. If the
check gives refused, the state becomes refused. A refused plugin is not retried by itself: the hub
does not watch files. `hub.admin.start` checks it again, so a repaired manifest applies at the next
`hub.admin.start` of that plugin.

**Order.** Hub start runs recovery in topological order. Ties keep config order: each step takes
the first entry in config order whose dependencies are listed. (Built: step 1, `startOrder`.) A
cycle member is refused, so the order always exists.

**Lifecycle.** There is no adoption exception. Inside recovery, plugin exit handling and
`hub.admin.start`, the order is:

1. Resolve spawn intents, as today. An unresolved intent still blocks a spawn, and it gives no
   registered current instance, so dependents wait.
2. Resolve the source and run the static graph check.
3. Only if step 2 passes: retire the old instance.
4. Spawn now, or enter `waiting` and spawn on wake.

A running dependent is never stopped by a later change of the graph. Example: A runs; its manifest
gets a cycle; A runs on, and its next start is refused. A dependency that stops or crashes later
does not stop its dependents. `hub.admin.start` of a plugin whose dependency is not registered
gives `waiting`.

**Repair entry point.** `hub.admin.start {"source": "<exact source string>"}` (beside
`{"plugin": name}`) resolves that entry again and starts it if the checks pass. Duplicate sources
are a malformed config, so a source names one entry. A hub restart works too.

**Run and candidate.** Each plugin has two records:

- `run`: the metadata of the current instance. The hub saves it in the plugins row when it spawns:
  source, dir, commit, digest, events, notice, depends. Exports come from that instance's
  register. Notices use `run.notice`. Readiness is the registration of `run`.
- `candidate`: the last resolution attempt, `{commit, dir}` or `{error}`. A resolution never
  writes into `run`. Only a spawn copies the candidate to `run`.
- `hub.plugins` reports state and commit from `run`, and `"candidate": {commit, error}` when the
  candidate is refused or names another commit. It also reports `source` and `dir`.

Example: P runs C1. An update locks C2, and C2 fails its graph check. The hub keeps `run` = C1.
`hub.admin.start` on a running plugin changes nothing (as today). To apply an update, run
`hub.admin.stop`, then `hub.admin.start`. In the example the start is refused, and the stop has
cleared `run` (no instance).

**Retained instance.** Hub recovery with a refused candidate does not retire the recorded block.
The hub marks the row `retained`.

- `hub.register` from a retained instance fails with code `retained`: `instance <id> of <name> is
  retained after a refused start`.
- A retained instance satisfies no dependency. Its dependents wait, or are refused when the static
  check refuses them.
- `hub.admin.stop` retires a retained block (as today).
- Source fact, not tested in a lab: Messaging, Profiles and `lazyrex.lua` read
  `rex.args.hub_client_id` at launch and register once at load. So an instance from an older hub
  calls the old hub client ID and cannot reach the new hub.

**Run.** The hub runs `rex do <dir>/<main>`, as today. A changed manifest name at a plugin start is
refused: `the manifest name changed from X to Y; restart the hub`.

## 8. Error texts

- `plugin messaging (github:lazy-rex/Messaging@main) is not installed: run lazyrex-hub plugin sync
  -config <cfg>`
- `plugin messaging: the cache has no entry for commit <c>: run lazyrex-hub plugin sync -config
  <cfg>`
- `plugin messaging: the cache entry for commit <c> changed (digest mismatch): run lazyrex-hub
  plugin sync -config <cfg>`
- `plugin source ../Messaging: no lazyrex.json in <abs dir>`
- `plugin messaging: main ../x.lua must be a file inside the plugin directory`

Sections 3 to 7 give the other texts.

## 9. Harness (`lazyrex-hub test`)

- The test loader reads the manifest of the plugin under test and each `test.plugins` manifest. It
  applies the same name, main, dependency and cycle rules. A failure is a harness error (exit 2)
  with the same text.
- The generated lab config has local `{"source": "<abs dir>", "args": ...}` entries only, so the
  lab needs no lock.
- `test.args` stays keyed by the manifest name.
- The harness adds the home plugin only when the plugin under test is not `lazyrex` (as today). A
  `test.plugins` entry named `lazyrex` is a harness error. Any other duplicate name among
  `test.plugins` and the plugin under test is a harness error.

**Home plugin.** It moves to `plugins/lazyrex/`, with `plugins/lazyrex/lazyrex.json` (name
`lazyrex`, main `lazyrex.lua`). Its tests move from `tests/home/` to `plugins/lazyrex/tests/`. The
messaging stub becomes `plugins/lazyrex/tests/stubs/messaging/`, with a manifest. Real configs
still list the home plugin as an entry. The config field `home` stays. `tests/harness-fail` and
`tests/isolation` get their own directories, because main must stay inside its directory.

## 10. Notice migration

1. Step 2a (built): the manifest parsers accept `notice`. The harness forwards it into the config
   field `plugins[].notice` of its lab config. Production start still uses the config's `notice`
   or the default text.
2. Messaging adds `"notice": "New message. Call messaging_read."` to its manifest.
3. The source-config change switches to the manifest notice and adds `no_notice` in one step, with
   no temporary fallback. `storeBatch` checks every op before its transaction. A `notify` op from
   a plugin without a notice fails with code `no_notice`: `plugin X has no notice in its
   manifest`. Nothing in that batch is written. There is no default text.

The live config changes format only at a release that the user approves. Local sources relative to
`~/.lazyrex` keep the release clones, for example `{"source": "release/Messaging"}`.

## 11. Steps

This repository, one branch per step:

1. Built: the source manifest loader and the graph checks (`sources.go`), with unit tests. The
   harness still has its own manifest parser (`testcmd.go`). Step 2 makes both use one loader.
2. Config source entries, removed fields refused, local resolution, the start graph, the states,
   `run` and `candidate`, the `hub.plugins` fields, the manifest notice and `no_notice`. The home
   plugin and self-test moves. `testcmd.go` hubConfig, `hub.example.json`, and the Go tests that
   write configs.
3. The lock, the cache, the digest check at start, `plugin sync|update|status`. Unit tests with a
   local bare repository (`file://`), so no test uses the network. A lab test for a start from a
   read-only cache entry.
4. Docs: hub.md, testing.md (the manifest table), README.

**Step 3 split (proposed).** The coordinator split step 3 into two parts (2026-10-21), so that two
pairs can work at the same time. Part A can start now. Part B starts after step 2e is on main.
Both parts are in package `main` of `hub/cmd/hub`.

- **Part A: the commands and the files** (sections 3 to 6). Part A owns:
  - the lock file format and its read and write (section 3), with the version check;
  - the config field `lock` and its default (section 1), in `Config`. The hub start accepts the
    field; a missing lock file is not an error;
  - the cache layout, publish and permissions (section 4), and the digest function;
  - refs and fetch (section 5): all git use;
  - the agreement check and the trusted-commit rule (section 3);
  - `lazyrex-hub plugin sync|update|status`, all or nothing, the graph check and the two flocks
    (section 6);
  - the error texts of sections 3 to 6, and the cache texts of section 8.

  Part A adds new files only (for example `pluginlock.go`, `plugincache.go`, `plugincmd.go`), plus
  the `lock` field in `main.go` and the `plugin` case in `main`. It reads the config with
  `parseEntries` and the manifests with `readPluginManifest`, and does not change them. It reads
  config entries only. When step 2e is on main, Part A adds the managed entries (section 13).
- **Shared files that Part A may change for the subdir form** (section 1): in `pluginconfig.go`,
  the `githubSource` pattern and the GitHub branch of `checkSource`, and new unexported fields of
  `configEntry` for the parsed parts (for example `owner`, `repo`, `subdir`, `ref`). No other code in
  that file. Step 2e also changes `pluginconfig.go`; the two changes do not overlap, and the part
  that lands second rebases.
- **Part B: the hub side.** Part B owns `resolveEntries` for a GitHub source: it replaces the
  "not installed" refusal with the calls below. It adds the commit to `pluginRun` and to
  `hub.plugins` (`run.commit`, `"candidate": {commit}`, section 7), and adds the lab test of a
  start from a read-only cache entry.

**The interface.** Part A gives these functions. Part B calls them and no other Part A code.

```go
// lockPath gives the lock file: the config field lock relative to the config directory, or the
// section 1 default. It reads no file.
func lockPath(cfgPath, field string) string

// readLock reads the lock file. A missing file gives an empty lock and no error. A file that
// does not parse, or has another version, gives an error.
func readLock(path string) (*pluginLock, error)

// lockedDir checks the lock entry of the GitHub entry e and its cache entry, without git and
// without the network: the key lookup, the agreement check (section 3), the cache entry, the
// manifest name and the digest (section 4). It returns the absolute plugin directory (the cache
// entry, or <entry>/<subdir> for a source with a subdir) and the locked commit. It only reads files.
func lockedDir(cfgPath, stateDir string, lock *pluginLock, e *configEntry) (dir, commit string, err error)
```

- The error of `lockedDir` is a final text of section 3 or 8, with the `-config <cfgPath>` of
  its command: no lock entry (`is not installed`), an agreement mismatch (`does not match:
  <field>`, with the `sync` or `update` text by the trusted-commit rule), no cache entry, and a
  changed cache entry (`digest mismatch`), and a cache entry that cannot be read (`plugin <name>:
  cannot read the cache entry <absolute dir>: <error>`; a file system error names the path and the
  error and does not name `sync`). Part B uses the text as the entry error unchanged.
- Part B calls `readLock` once in each resolution (each start attempt re-reads the lock, section
  7), and `lockedDir` once for each GitHub entry, with that one snapshot. `lockedDir` never reads
  the lock file again. Neither function writes a lock file or a cache entry.
- A missing lock file gives an empty lock: each GitHub entry is "not installed" (section 8).
- A `readLock` error refuses every GitHub entry. The hub start does not fail (section 1: only a
  malformed config fails the start), and local entries resolve as usual. The graph rules refuse a
  local dependent of a refused GitHub entry. The ownership and retained-instance rules apply. The
  text depends on the error:
  - The file does not parse, or has another version: `plugin <source>: the lock file <path> is
    not valid: <error>; repair the file or restore it from a backup`. Such a lock gives no
    trustworthy pins, so the text does not name `sync`. `sync` and `update` refuse it too, and
    never replace it: a lock that does not decode is never taken as an empty lock.
  - A filesystem error (for example, no read permission): `plugin <source>: cannot read the lock
    file <path>: <error>`.
  - Only a missing entry and a repairable entry with a trusted commit name `sync` (section 3).
- After `lockedDir`, Part B reads the manifest with `readPluginManifest(dir)`, as for a local
  source, so the manifest rules and the graph are the same for both kinds of source.
- No hub code takes a flock (section 6). The hub reads the lock with one read. The commands
  replace the file with a rename, so the hub reads the old lock or the new lock.
- For tests, the commands are also Go functions with an option for the remote URL, so a test can
  use a local bare repository (`file://`) in place of GitHub:

  ```go
  type pluginCmdOpts struct {
      // remote gives the fetch URL of github:<owner>/<repo>. Default: https://github.com/<owner>/<repo>.
      remote func(owner, repo string) string
      // afterResolve, when set, runs after a ref is resolved and before the fetch (a test of a ref
      // that moves during the fetch). Production code never sets it.
      afterResolve func(source string)
  }
  func pluginSync(ctx context.Context, cfgPath string, o pluginCmdOpts) error
  func pluginUpdate(ctx context.Context, cfgPath string, args []string, o pluginCmdOpts) error
  ```

  The Part B lab test calls `pluginSync` in the test process to make the lock and the cache. Then
  it starts the hub binary, which reads them. The remote option and the afterResolve hook exist
  only in these functions: no CLI flag, config field or environment variable sets them.
- The arguments of `update` (step 2e rule, approved 2026-10-21): an argument selects entries by
  three keys: the plugin name, the exact source string, and `managed:<id>`. An argument that
  starts with `managed:` selects only by managed id; it never matches a name or a source. (A local
  config source literally named `managed:...` is selected by its plugin name.) The selector counts
  distinct entries: one entry that matches by name and by source counts once. It must select
  exactly one entry. Zero: `plugin update: no entry has the name, source or identity <arg>`. Two or
  more: `plugin update: <arg> selects <identities>; write managed:<id> or the exact source`, with
  the identities `entry N` and `managed:<id>`.
- A change of these signatures or of the error texts needs the agreement of both parts and of
  their reviewers.

## 12. Acceptance matrix

U = Go unit test, L = Go lab test, H = Lua harness test.

| Case | Expected | Test |
| --- | --- | --- |
| A local source, relative to the config directory | starts from that directory | U, H |
| A removed field (`path`, `name`, `events`, `notice`) in an entry | hub start fails; the text names the field | U |
| Two entries with the same source | hub start fails | U |
| main with `..`, absolute, or a symlink (also pointing inside) | refused | U |
| A symlinked parent that resolves inside; outside | accepted; refused | U |
| No lazyrex.json | refused; entry and source identity; name null | U |
| Duplicate names (two local entries; local and lock) | both refused | U |
| A missing, refused (transitive) or cyclic dependency | refused with the texts | U |
| A dependency registers after the dependent waits | the dependent spawns | H |
| A runs; its manifest gets a missing dependency; A crashes | refused; the old block is kept, not retired | L |
| A refused manifest is repaired; `hub.admin.start` | starts | H |
| Hub start order | dependencies spawn first; dependents spawn after register | H |
| An unresolved spawn intent of a dependency | the dependent waits | U |
| A hub restart with a refused candidate | the old process lives (pid and start time); its block stays; the row is retained; a dependent waits, or is refused when its check refuses | L |
| `hub.register` with a retained instance ID | code `retained` | U |
| A GitHub source without a lock entry | refused, "not installed" | U |
| sync: a new entry; a missing cache; a changed cache | the lock gets the entry; a new dir; the running dir is untouched | U (`file://`) |
| sync fails on the second of two plugins | the lock is unchanged; no referenced dir changed | U |
| update moves a branch; the ref moves during the fetch | a new commit in the lock; refused, the lock is unchanged | U |
| A suffix-only ref match; branch and tag; annotated tag through the real fetch path; invalid ref; 40-hex that is not a commit; unavailable commit | refused, or peeled as specified | U |
| An executable helper; a symlink; a submodule in the tree | kept 0555 and the digest passes; refused; refused | U |
| A lock agreement mismatch (each field) | refused; the text names the field | U |
| A trusted and an untrusted locked commit | sync rebuilds the fields; sync refuses with the update text | U |
| Two commands on one state with two lock files | the second fails at once | U |
| Hub start during a sync | the old or the new lock, never a mix | U |
| A start from a read-only cache entry that dofiles siblings | runs | L |
| `hub.admin.start {source}` after a repair | the entry resolves and starts | H |
| A notify from a plugin without a notice | `no_notice`; no record written | U, H |
| Harness: the home plugin under test; a `test.plugins` duplicate or `lazyrex` | one home plugin; a harness error | U |

## 13. Managed plugins (proposed for step 2, not approved)

The plugin manager (the hub features pair) adds and removes plugins without editing the user's
config. Step 2 must read its file from the start. If it does not, a user's `plugin sync` drops the
lock entries of managed plugins. The coordinator's split: this section goes into step 2. The hub
writing lock entries, `plugin fetch`, the pinned-target start and the `spawn_intents` kind stay
with the features pair, on top of step 2.

**File.** Config field `managed_plugins`: a path relative to the config directory. Default:
`<state>/plugins.json`.

```json
{"plugins": [{"id": "<id>", "source": "<abs dir or github:...>", "args": {}, "watch_roots": []}]}
```

- `id`: `[a-z0-9_-]{1,64}`, required. It is the manager's handle for the entry. It is not the
  plugin name: the manifest owns the name.
- `source`: an absolute local directory or a `github:` source. A relative path is refused: the
  file has no config directory to resolve against.
- `args`: as in a config entry.
- `watch_roots`: an array of absolute, clean paths. Step 2 checks only this shape and keeps the
  value. Its meaning belongs to the plugin manager design.
- Unknown fields are refused. This rule also keeps the fields `user_messaging` and `prompts` out of
  the managed file: only the user's config can set them (user decision, 2026-10-19). They are not
  hub fields yet; the plugin manager design names them. A new managed field needs a design change.

**Missing and malformed file.**

- A missing file is an empty list. The manager may never have written it. Only a file with an
  empty `plugins` array is another empty list.
- A malformed file: bad JSON, a root that is not an object with a `plugins` array (also `null`,
  `{}` and `{"plugins": null}`), an unknown field, a bad `id` or source, two entries with the same
  `id`, two entries with the same source, or a file over 1 MiB (1048576 bytes; the coordinator,
  2026-10-21; the plugin manager refuses to write a larger file).
  - Hub start does not fail. The hub starts the config plugins and refuses every managed entry.
    `hub.plugins` reports the file error once, as `{"managed_file": "<path>", "error": "..."}`.
    Reason: a defect of the manager must not stop the plugins that the user configured.
  - `plugin sync`, `update` and `status` fail, and the lock stays unchanged. Reason: a command
    that ignored the file could drop the managed lock entries.

**Identity and graph.**

- A managed entry's identity is `managed:<id>` and its exact source string. Texts use it, for
  example `entries 2 and managed:notes have the same name notes`.
- A source in both the config and the managed file: the managed entry is refused, and the config
  entry is checked as usual. Text: `managed:<id>: the source <source> is also in the config`.
- The graph (section 7) contains the config entries and the managed entries. Names, dependencies
  and cycles are checked across both.
- Order: config entries first, then managed entries in file order, for the tie rule of
  `startOrder`.
- `hub.admin.start {"source": ...}` names a managed entry too.

**Reload (open question, for the step 2 plan).** The approved design (section 7) re-reads the
manifests and the lock at each start attempt. It does not reload the config entry list: a config
change needs a hub restart, as today. When the hub reads the managed file again is not decided
yet. The plugin manager needs a reload while the hub runs. Any reload rule must keep these
conditions:

- A reload keeps `run` (the running-instance metadata) and unresolved spawn intents when an entry
  changes or disappears.
- A malformed managed file is never a request to stop running instances or to delete lock
  entries.

**Commands.** `sync`, `update` and `status` read the config entries and the managed entries.
`update` accepts a managed `id` as well as a name or a source.

**Orphaned lock entries.** A lock entry whose source is in neither the config nor the managed
file never starts a plugin. `plugin status` reports it as `orphaned`. `plugin sync` removes it.
For example, a cancelled add of the manager can leave one.

**Size bounds.** A step 2 decision of the coordinator (2026-10-20), separate from the v3 design
approval: the hub refuses a `lazyrex.json` over
64 KiB, with one shared constant and an error that names the file and the size. The hub has no
main-file bound. Reasons:

- Every current plugin is far below the bound (the table below).
- An unbounded manifest read is a memory and reply-size risk. Compare the scan outage of
  2026-10-19 (hotfix f125044).
- The plugin manager needs the same bound.

The features pair first proposed a 1 MiB main-file bound as well. It stays in the manager's
preview only. Measured on 2026-10-20 (the current main of each repository):

| Plugin | lazyrex.json | main file |
| --- | --- | --- |
| Messaging | 459 B | 123461 B |
| Profiles | 188 B | 25647 B |
| Templates | 237 B | 61113 B |
| Tunnels | 367 B | 66160 B |
| Worktrees | 588 B | 74677 B |
| lazyrex (home) | none yet | 11241 B |

How step 2 applies the decision:

- Manifest, 64 KiB. The hub reads the whole manifest at each start attempt, so the bound keeps
  that read small. The largest manifest today is 588 B, about 1/100 of the bound. Proposed error:
  `<dir>/lazyrex.json is <n> bytes, over the 65536-byte limit`.
  - The read itself is bounded: the loader reads at most 65537 bytes. A length check after an
    unbounded read does not meet the memory goal.
  - Tests: a manifest of exactly 65536 bytes is accepted; 65537 bytes is refused.
- Main file: no hub bound. The hub does not read the main file: it checks its type, and `rex do`
  reads it. (The digest reads every file, as a stream.) Whether Rex limits the size of a `rex do`
  script: not tested.
- The constant lives with the manifest loader, so the manager and the hub use one value.

## 14. Not in this design

- The lock can replace the release clones: a release is `plugin update <name>` and a plugin
  restart; a rollback is a lock entry with an older commit. This needs a user decision.
- `depends` has no consumer of files today. If a plugin needs its dependency's files, the hub can
  pass the dependency directory as an arg later.
- Hot reload of local sources belongs to the dev.sh rewrite. dev.sh can read the plugin directories
  from `hub.plugins`.

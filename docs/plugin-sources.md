# Plugin sources

This document is the reference for plugin sources: where a plugin comes from, how the hub pins and
caches a GitHub source, and how the hub starts, replaces and reports a plugin. docs/hub.md gives
the hub actions in short form and points here.

Code:

| Part | Files (`hub/cmd/hub`) |
| --- | --- |
| Config entries, resolution | `pluginconfig.go`, `sources.go` (manifest and graph rules) |
| Lock, cache | `pluginlock.go`, `plugincache.go`, `digest.go` |
| Refs, fetch, `@latest` | `pluginfetch.go`, `pluginlatest.go` |
| Lock writers, pin document, `lazyrex-hub plugin new` | `plugincmd.go`, `pinsync.go`, `pluginnew.go` |
| Start, states, kept records | `plugins.go`, `respawn.go`, `reload.go` |
| Selected run, local snapshots | `selectedrun.go`, `localsnapshot.go` |
| `expect_run`, replace | `expectrun.go`, `replace.go` |

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
- `prompts` and `user_messaging`: booleans, false when absent. They grant the agent prompts and
  the user's Messaging identity (docs/hub.md, "Agent prompts and acting as the user"). Only the
  user's config sets them. Two entries with `user_messaging` are a malformed config.
- A source that starts with `github:` is a GitHub source. Any other source is a local directory.
- A local path is absolute or relative to the directory of the config file. `~` is refused.
- `<owner>` matches `[A-Za-z0-9-]+`. `<repo>` matches `[A-Za-z0-9._-]+` and is not `.` or `..`.
- `<subdir>` is optional: the plugin directory inside the repository, in the form of a Go module
  path, for example `github:lazy-rex/LazyRex/plugins/lazyrex@main`. It is one or more segments
  separated by `/`. Each segment matches `[A-Za-z0-9._-]+` and is not `.` or `..`. No segment is
  empty, and there is no leading or trailing `/`. Owner, repo and subdir never
  contain `@`, so the first `@` ends the path and the ref is the rest. (A ref can contain `#`, so a
  `@ref#subdir` form would be ambiguous.) Text: `source <s>: subdir <d> must be a relative path of
  names (A-Z, a-z, 0-9, ., _, -)`.
- `<ref>` is required. See section 5 for its forms. The ref `latest` is reserved: it selects the
  highest release tag (section 5, "@latest").
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

Fields: `name`, `main`, `sdk`, `events`, `notice`, `depends`, `test`. The hub refuses unknown fields.
The file is one JSON object; data after the object is refused.

- `name`: `[a-z0-9_-]+`, required.
- `main`: required. It is relative to the manifest directory, has no `..` component and is not
  absolute.
  - `Lstat(main)` must be a regular file. A symlink main is refused, also when it points inside the
    source.
  - A parent directory can be a symlink (for example a release clone path).
    `EvalSymlinks(main)` must be inside `EvalSymlinks(source dir)`.
- A plugin is its own directory: the manifest directory (its subdir for a GitHub source). A plugin
  loads only files inside its own directory. A read outside it (`../x.lua`, an absolute path to
  another tree) is not supported. The hub does not refuse such a read; the contract does not cover
  it. A local source runs from a snapshot that holds only its directory (section 4). A relative
  path such as `../x.lua` resolves from the snapshot, so it does not find a sibling of the source
  directory. A plugin finds its files from its own run path (debug info, or the SDK's plugin
  directory), never from its source path.
- `sdk`: `"0"` or absent. With `"0"`, the hub starts the plugin through the SDK launcher:
  `rex do <sdk dir>/boot.lua main=<absolute main> ...` (docs/plugin-sdk.md). The launcher puts the
  plugin directory and the SDK directory in `package.path`, so `require('lazyrex')` and a sibling
  `require` work. The hub gets the SDK directory from `-sdk` (dev.sh and `lazyrex-hub test` pass
  `<checkout>/sdk/lua`). Without a directory that holds `boot.lua`, the plugin is refused. The arg
  names `main` and `lazyrex_sdk` are reserved. Without `sdk`, the hub runs `rex do <absolute
  main>` (section 7, "Run").
- `events`: `session` (default) or `all`.
- `notice`: an optional string. It is behavior, so it belongs to the plugin. See section 10.
- `depends`: an array of plugin names, for example `["profiles"]`. A name appears once. There is
  no version rule: the lock pins exact commits. An object form can be added later.
- `test.plugins`: replaces `requires`. It is `[{"source": "<dir>"}]`, local directories only,
  relative to the manifest directory, each with its own manifest. The harness reads it; the hub
  does not. A stub can satisfy a dependency by its manifest name.

`requires` is an unknown field, so it is refused.

## 3. Lock

```json
{"version": 1, "plugins": {"<exact source string>": {
  "name": "", "owner": "", "repo": "", "subdir": "", "ref": "", "ref_kind": "branch|tag|commit",
  "commit": "<40 hex>", "digest": "sha256:<hex>", "dir": "github/<owner>/<repo>/<commit>.<token>"}}}
```

A lock with a latest entry is version 2. Its other entries keep the form above.

```json
{"version": 2, "plugins": {"github:<owner>/<repo>@latest": {
  "name": "", "owner": "", "repo": "", "subdir": "", "ref": "latest", "ref_kind": "latest",
  "tag": "v1.2.0", "commit": "<40 hex>", "digest": "sha256:<hex>",
  "dir": "github/<owner>/<repo>/<commit>.<token>"}}}
```

- `version` follows the content of the file. A lock with a latest entry (`ref_kind` `latest`, or
  a `tag` field) is version 2. Every other lock is version 1, with the bytes of a hub before
  `@latest`: no `tag` field. When the last latest entry goes, the lock is version 1 again. A hub
  reads versions 1 and 2. A version 1 lock with a latest form is refused: `the entry for <source>:
  a latest entry needs version 2`. Another version: `version N is not supported (this hub reads
  versions 1 and 2)`.
- `tag` is the release tag that `@latest` selected (section 5). Only a latest entry has it; it is
  absent, never empty or null, on every other entry.
- The key is the source string exactly as the config writes it. A changed source string (for
  example `@main` to `@v2`) is a new key: the hub installs it. It is not an update of the old key.
- `dir` is relative to `<state>/plugins`.
- `subdir` is the subdir of the source, or "" without one. A version 1 lock entry without the
  field decodes as "": the version stays 1. The same repository at two subdirs gives two keys and
  two cache entries.

**Agreement check.** The hub checks a lock entry before it uses it:

- the key equals the config source;
- `owner`, `repo`, `subdir` and `ref` equal the parsed source;
- `ref_kind` matches the ref form;
- `commit` is 40 lowercase hex, and equals the ref when the ref is a commit;
- a latest entry has a `tag` of the release form (section 5); no other entry has a `tag`;
- `dir` is `<owner>/<repo>/<commit>.<token>` under `github/`;
- the manifest name in the cache entry equals the lock `name`;
- the digest matches.

A mismatch refuses the entry: `the lock entry for <source> does not match: <field>; update the
plugin in LazyRex: Plugins…`.

**Repair.** The key and the commit are the pin; for a latest entry, the key, the commit and the
`tag` are the pin. Every other field is derived from them.

- A commit is **trusted** when it is 40 lowercase hex and, for a commit ref, equal to the ref. The
  commit of a latest entry also needs a `tag` of the release form: an install cannot rebuild the
  tag from the commit.
- The install of a missing or changed cache entry keeps the locked commit (and the locked tag of a
  latest entry) and fetches that exact commit again: it publishes a new dir, computes the digest,
  and reads the name from the fetched manifest.
- An untrusted commit is malformed, not equal to a commit ref, not a commit, or not available. The
  hub refuses that entry: `the locked commit for <source> cannot be trusted: <reason>; update the
  plugin in LazyRex: Plugins…`. An update pins a new commit.

## 4. Cache

- An entry path is `<state>/plugins/github/<owner>/<repo>/<commit>.<token>/`, where `<token>` is
  16 random hex. The lock entry records the path.
- The entry is the whole commit tree, also for a source with a subdir. The digest covers the whole
  entry. The plugin directory is `<entry>/<subdir>` (the entry itself without a subdir). The main
  file rule (main inside the plugin directory) applies to that directory. The entry holds the whole
  commit, but a file outside the subdir is not part of the plugin (section 2), and a plugin that
  loads one does not run as a local source.
- Published permissions: files 0444, executable files 0555, directories 0555.
- **A published entry never moves.** No command renames, deletes or writes a published entry.
  - A repair publishes a new entry and points the lock at it.
  - A running instance keeps its directory.
  - A changed entry stays on disk. An entry that the lock does not reference is not used.
- **Publication** (observed on macOS 27.0.1, APFS; Linux: not tested). A rename of a 0555
  directory into another parent fails with EACCES, because the move must update the directory's
  `..`. A rename in the same parent works. So the command extracts the tree into a stage beside
  the final path, `github/<owner>/<repo>/.stage-<token>/`, sets every published mode (the stage
  root last), and renames the stage to `<commit>.<token>` in the same parent. Nothing writes the
  entry after the rename. A failure before the rename removes the stage. Names that start with
  `.` are stages, not entries.
- There is no prune. A running plugin can `dofile` a sibling file later (Tunnels does), so the hub
  must not delete an entry that is in use. A future prune keeps every entry that a lock names, or
  that a plugins row names in `run` or in `selected_run` (section 7, "Selected run"). A local
  snapshot (`local/<hex>.<token>`, below) has no lock, so for it the rule is `run` or
  `selected_run`. It also needs a guard against a start that reads a lock file or copies a local
  source while prune runs: the hub has none.
- **Local snapshots** (`localsnapshot.go`). An explicit start of a local source copies the source
  directory, and only that directory (section 2), into a stage, `local/.stage-<token>/`, and
  publishes it as `local/<hex>.<token>`, where `<hex>` is the digest of the published bytes (the digest rule below). `.git` is skipped. A symlink or
  another non-regular file refuses the start (`plugin <name>: <path> is not a regular file; a local
  source is copied, and the copy has no symlinks`). The bound is 10,000 files and 64 MiB; over it,
  the start refuses with the numbers. The modes are the modes of a cache entry. An existing
  `local/<hex>.*` entry is used only if its digest verifies; a bad entry is never used or changed,
  and the start publishes a new token. The hub reads the manifest again from the snapshot: a
  change of name, main, sdk, events, notice or depends refuses (`the source changed during the
  start; start it again`). The run's `dir` is the snapshot, and its `digest` is the snapshot's. A
  start that fails after the publication stores no selected run and leaves the new entry for the
  prune; the hub never removes an entry. A plugin must not write into its own directory: the
  snapshot is read-only.
- **Source runs** (test only). The hub config key `test_source_runs` is the only exception to "the
  run's `dir` is the snapshot". It lists local source dirs. The hub accepts it only with `-test`,
  and each dir must be in `test_roots`; a release rehearsal refuses it in a live config. A start
  of a listed source runs from the source dir: no snapshot, and the run has no `entry` and no
  `digest`. `lazyrex-hub test` lists only the plugin under test (section 9), so `${PLUGIN}` is the
  dir it runs from. The home plugin and the `test.plugins` dependencies run from snapshots. Two
  test-only limits follow from the missing digest: `hub.admin.start` of a running source-run
  plugin keeps the instance (there is no digest to compare, section 14), and the run check's
  refusal text has an empty run identity (`run  is no longer available`).
- A future prune command also removes `.stage-*` and `.tmp/*` entries older than one hour (a CLI
  fetch ended by SIGKILL, or a manager fetch whose output did not finish). Until it exists, they
  stay as unused disk state; no lock names them.
- **Digest**: sha256 over the lines `<path>\0<x|->\0<size>\0<sha256 hex of content>\n`, sorted by
  path bytes. `x` is mode 100755. The command computes it from the archive, before it changes the
  permissions. At each plugin start the hub computes it again; there `x` is "any execute bit
  set".
- A plugin start never runs git and never uses the network: it reads the config, the lock and the
  cache. Git runs only in the plugin manager's ops (add, update and the hub's install, docs/hub.md
  "Plugin operations"), in a fetch child.

## 5. Refs and fetch

The plugin manager's ops use the git CLI. A plugin start
never runs git. Git runs with
`GIT_TERMINAL_PROMPT=0`. It uses the user's credential helper and `url.insteadOf` (for SSH).

**Ref forms**: `latest`, `<name>`, `refs/heads/<name>`, `refs/tags/<name>`, or 40 lowercase hex.
The ref `latest` has the kind `latest` only (see "@latest" below): a branch or a tag named
`latest` needs its full ref, `refs/heads/latest` or `refs/tags/latest`. A short hex is a name, not
a commit. `git check-ref-format` validates `refs/heads/<name>` and
`refs/tags/<name>`. Error: `github:o/r@<ref>: <ref> is not a valid ref`.

**Resolve**: `git ls-remote <url> refs/heads/<n> refs/tags/<n> refs/tags/<n>^{}`.

- The command keeps only lines whose name equals one of these three. A suffix match
  (`refs/heads/team/<n>`) is dropped.
- A name that is both a branch and a tag is refused: `github:lazy-rex/Messaging@foo: ref foo is
  both a branch and a tag: write refs/heads/foo or refs/tags/foo`.
- An annotated tag resolves to its `^{}` commit.
- A 40-hex ref is not resolved.
- No line matches: `github:o/r@<ref>: ref <ref> is not found`.

**@latest** selects the highest release tag that points to a commit (`pluginlatest.go`).

- A release tag matches `^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`: `v`, then three
  decimal numbers without a leading zero. A pre-release (`v1.0.0-rc1`), build metadata, an
  upper-case `V` and every other name are not release tags.
- Order: major, then minor, then patch, as decimal numbers with no length limit (`v1.10.0` is
  higher than `v1.9.0`).
- `git ls-remote --tags -- <url> refs/tags/v*` lists the candidates. ls-remote gives no object
  types. So the command fetches each candidate, highest first, into a bare repository in
  `<state>/plugins/.tmp/<random>/` (`git fetch -q --no-tags --depth 1`), and reads `git cat-file
  -t <tag ref>^{}`. The `^{}` peels every level of annotated tags.
  - `commit`: this tag is the release.
  - `tree` or `blob`: the command skips the tag and names it in an `ignored` line (section 6).
  - Any other type: `refs/tags/<t> points to the object type "<type>"`.
  - A git failure fails the resolution. A failure never selects an older release.
- No release tag points to a commit: `the repository has no version tag vX.Y.Z that points to a
  commit`. There is no fallback to a branch.
- Then the release goes through the fetch below, by its tag ref: a tag that moves after the
  selection is refused (`moved during the fetch`). The lock entry gets `ref` `latest`, `ref_kind`
  `latest`, `tag` and the commit.

**Fetch** into a bare repository in `<state>/plugins/.tmp/<random>/` (`git init -q --bare`, then
`git fetch -q --depth 1`). The tree is extracted into the stage of section 4, "Publication":

1. Fetch a branch or tag by its exact ref. Fetch a commit (a 40-hex ref, or the install of a
   locked commit) by its ID.
2. Read `git rev-parse --verify FETCH_HEAD^{commit}` and compare it with the resolved commit. Raw
   `FETCH_HEAD` is never compared. A difference: `ref <n> moved during the fetch; run the
   operation again`. There is no retry loop.
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
- The fetch refuses an archive that differs from the tree. `.gitattributes` `export-ignore` and
  `export-subst` cause that refusal when they take effect: `export-ignore` on a file that is in the
  tree removes it, and `export-subst` on a file with a `$Format:...$` marker changes its content.
  An attribute that changes nothing (no matching file, no marker) does not refuse the source.
- With a subdir, `<subdir>/lazyrex.json` must be in the commit. `main` must be inside the subdir.
  The whole commit is in the cache entry, but a plugin reads only files inside its subdir (section
  2): a read outside it is not supported, and a local snapshot does not contain it.
- The cache entry is read-only (files 0444 or 0555). A plugin writes its data elsewhere, for
  example in the hub store.

## 6. Lock writers

The plugin manager writes the lock: its install, add, update and remove ops (docs/hub.md, the
`locking` and `unpinning` phases). They run in the hub's op manager, not in a hub action, because
a hub action must answer fast. There is no CLI command that writes the lock.

- An install fetches a GitHub source that has no lock entry (it resolves the ref), or the locked
  commit again when its cache entry is missing or changed (section 3, "Repair"). It never moves a
  locked commit. A latest source: the first install selects the highest release (section 5); a
  repair fetches the locked commit by its ID and keeps the locked tag, also when the tag is gone
  from the repository.
- An update resolves the ref again and pins the result.
- A lock entry that no entry names stays; it is not used.
- `lazyrex-hub plugin new <name> [-dir <path>]` writes a new SDK plugin (`lazyrex.json`,
  `main.lua`, `tests/<name>.lua`, `README.md`) into a directory that must not exist (default
  `./<name>`). The files go into a stage beside it, then one rename publishes them: the directory
  gets every file or none. It needs no config, and it does not write the lock.

**All or nothing.** A lock writer writes the lock only after every fetch and check passes (temp
file, fsync, rename, directory fsync). A failure before the rename leaves the old lock. Entries
that the writer published stay on disk; nothing references them. So a failed write never changes a
directory that a lock references. A failure of the directory fsync comes after the rename: readers
see the new lock, but a crash can still undo it. The writer then fails with `<lock> is replaced,
but the sync of its directory failed (a crash can undo the change): ...`, never claims an
unchanged lock, and rolls nothing back.

**Graph check.** Before it writes the lock, the writer checks the new dependency graph (docs/hub.md,
the `locking` phase).

**Concurrency.** A lock writer takes two exclusive flocks, the plugin command flocks, in this
order:

1. `<state>/plugins/.lock`: all writers on one state, for any config or lock file.
2. `<lockfile>.lck`.

A second holder fails at once: `another plugin command runs (pid N)`. N can be the pid of this hub
or of another hub on the same state. An op that finds the flocks held waits in its phase and tries
again. The export of the pin document (section 6a) takes both flocks too. A rename replaces the
lock, and cache entries are immutable, so a hub start reads the old lock or the new lock, never a
mix.

## 6a. Pin sync

The machines of the user select the same pins (commit and digest) for the GitHub sources that
their configs share. The hub of one machine reads the pins of a peer hub over federation
(docs/hub.md, "Plugin sync"), and its plugin manager applies them as update ops. A running
instance keeps its run commit until the op replaces it (section 7).

Each machine fetches each commit from GitHub itself. No cache entry moves between machines. The
digest shows that two machines have the same tree. Observed for one tree
(`github:lazy-rex/LazyRex/plugins/lazyrex@ddc3e1b12def77eb5f23779e1528148fd8a7dbef`, hub
a711415): macOS 27.0.1 arm64 with git 2.54.0 and umask 022, and Linux 6.8.0 x86_64 with git
2.43.0 and umask 0002, gave the same digest, the same 275 files and the same 5 executable files.

Note: `hub.plugins` shows each entry's exact source, commit and digest to any client that can call
the hub, and a source name can name a private repository.

**The pin document.**

```json
{"version": 1, "hub": "<32 lowercase hex>", "entries": [{"source": "<exact source>",
  "name": "", "owner": "", "repo": "", "subdir": "", "ref": "", "ref_kind": "", "commit": "", "digest": ""}]}
```

A document with a latest entry is version 2. Its other entries keep the form above.

```json
{"version": 2, "hub": "<32 lowercase hex>", "entries": [{"source": "github:<owner>/<repo>@latest",
  "name": "", "owner": "", "repo": "", "subdir": "", "ref": "latest", "ref_kind": "latest",
  "tag": "v1.2.0", "commit": "", "digest": ""}]}
```

- `version` follows the content, as for the lock (section 3): 2 when an entry is a latest entry,
  else 1, with no `tag` field. A hub reads 1 and 2. A version 1 document with a latest entry is
  refused: `entry N (<source>): a latest entry needs pin document version 2`. Another version:
  `pin document version N is not supported by this hub (it reads versions 1 and 2)`.
- `tag`: only on a latest entry, and required there. Texts: `tag is missing`, `tag "<t>" is not a
  version tag vX.Y.Z`, `tag is set, but ref_kind is <kind>`.
- The entries are sorted by source. The same pull identity (`hub`) and the same pins give the same
  bytes; two locks with the same pins give different documents. The document has no path
  and no cache token. An empty document has `"entries": []`.
- The reader is strict. Another version gives `pin document version N is not supported by this
  hub`. Unknown fields and a source that is there twice are refused.
- Each entry must agree with its source: owner, repo, subdir and ref equal the parsed source;
  `ref_kind` is a kind that the ref form allows (section 5); the commit is 40 lowercase hex and
  equals a commit ref; name and digest have the lock forms. One bad entry refuses the whole
  document, before any fetch.

**The sidecar.** `<lock>.id` lives beside the lock and belongs to the lock file: two configs that
name one lock share it. It is the pull identity of the lock, the `hub` of the documents it
exports. The first export makes it (a new file, fsync). A malformed file: `<path> is not valid:
...; remove it to make a new pull identity`. A stray `<lock>.peers.json` is ignored.

**Export.** Under both flocks, export reads the lock and the managed file as one snapshot. It
exports each GitHub entry of the config and of the managed file, and only when each one passes
`lockedDir`: a machine never exports a pin that it cannot run itself. Otherwise it fails with that
entry's text, writes nothing and makes no pull identity. An orphaned lock entry and a local source
are not exported.

## 7. Start, states and dependencies

**Entry identity.** Each entry has an identity: its index in the config and its exact source
string. Its name is known only from a local manifest, or from a lock entry that passed the
agreement check.

- An entry without a known name is reported as `{"entry": 3, "source": "...", "name": null,
  "state": "refused", "error": "..."}`. The hub keeps such entries in a list beside the name map,
  never under an invented name.
- Duplicate names: every entry with that name is refused: `entries 2 and 4 have the same name
  messaging`. An entry that resolves later and finds its name in use is refused the same way.
- Each start attempt resolves every entry again (`resolveEntries`). A name binds at the first
  resolution that gives it without a conflict. A duplicate name refuses every entry with it, also
  when one entry resolved earlier: that owner keeps its name and a running instance; the other
  entry binds nothing. Two entries with one name at hub start: neither binds.

**Kept records.** The plugins row records the source of the entry that bound its name (column
`source`). Each record loads its plugins row when the hub makes it, before an action can see it.
The admin actions load the row again under the record's lifecycle lock.

- At hub start, each row with a recorded block or an open spawn intent gives a record, also when
  no entry gives its name now.
- An entry adopts that record when it gives the same name from the same source. A row without a
  recorded source matches by name only.
- A record that no entry adopts is kept. It never starts or respawns, and it gets no notices.
  `hub.register` from its instance fails (code `retained`). Its state is `retained` (the child
  lives) or `refused`.
- `hub.admin.stop` of a kept record resolves its intents first (an unresolved one keeps the
  record: code `unresolved_intent`), then retires its block and drops the record.
  `hub.admin.start` of a kept record is refused.
- Recovery skips a record that `hub.admin.start` already started in this hub process.

**Ownership guard.** An entry whose source a kept record holds under another name, or whose name
a kept record holds from another source, is refused ("stop X first") and binds nothing. Its name
still counts in the duplicate check. After the stop of X, `hub.admin.start {source}` starts the
entry.

**The graph.** Each explicit start rebuilds the whole graph. An explicit start is a hub start,
`hub.admin.start`, a reload or a replace. The start re-reads every local manifest and the lock,
and verifies the cache entries. Then it applies the refusal rules to the plugin to start (an
automatic start uses the selected graph instead: "Selected run" below):

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
- `crashing`: desired running; the recorded instance has exited; a respawn is due at `respawn_at`
  (the crash backoff, hub.md). It is not `stopped` and not `running`.

**Wake rule.** The hub checks a waiting plugin again when any plugin registers, when
`hub.admin.start` or `hub.admin.stop` runs for any plugin, and when a plugin is refused. If the
check gives refused, the state becomes refused. A refused plugin is not retried by itself: the hub
does not watch files. `hub.admin.start` checks it again, so a repaired manifest applies at the next
`hub.admin.start` of that plugin.

- A dependency counts as registered when it has a config entry, is not retained, has a client
  from `hub.register` and a live child. An unresolved spawn intent gives no client, so the
  dependent waits.
- Waiting makes no spawn intent and changes no recorded ownership. The event
  `plugin_waiting {plugin, waiting}` comes when the names change.
- The wake loop (`waitLoop`) runs the start path again for each waiting plugin, in config order. A
  wake during a scan makes one more scan.
- A stop of a waiting plugin ends the wait. A stop, a refusal and a spawn clear the names.
- `hub.admin.start` of a plugin that must wait gives `{"desired": "running", "state": "waiting",
  "waiting": [names]}`.

**Order.** Hub start runs recovery in topological order (`startOrder`). Ties keep config order:
each step takes the first entry in config order whose dependencies are listed. A cycle member is
refused, so the order always exists. A dependency spawns first; its dependent waits and spawns
after the dependency registers.

**Lifecycle.** The explicit start path, `startPlugin` (`plugins.go`), serves hub start recovery
and `hub.admin.start`; a reload and a replace use the same checks. An automatic start takes
`startAutomatic` ("Selected run" below). There is no adoption exception. The order of the
explicit start path is:

1. Resolve spawn intents. An unresolved intent blocks a spawn, and it gives no registered current
   instance, so dependents wait. `hub.admin.start` does not abandon open intents: an unresolved
   intent fails the start with code `unresolved_intent`. The text names the instance and the
   session, because a `client.reply` error has no other fields (rex-protocol.md, "Client RPC").
2. Resolve the source and run the static graph check.
3. Only if step 2 passes: retire the old instance.
4. Spawn now, or enter `waiting` and spawn on wake.

A running dependent is never stopped by a later change of the graph. Example: A runs; its manifest
gets a cycle; A runs on, and its next start is refused. A dependency that stops or crashes later
does not stop its dependents. `hub.admin.start` of a plugin whose dependency is not registered
gives `waiting`.

**Selected run** (`selectedrun.go`). An automatic start (a crash respawn, a restart after a
session loss, a wake) runs the selected run: the run that the last explicit start selected. It
reads no pin, so a lock change or a local edit takes effect at the next explicit start.

- The plugins row stores it (column `selected_run`) with the identity of the entry that selected
  it. A spawn sets it in the save of the run; an explicit start that waits sets it before its
  lease ends.
- A crash, a failed spawn, a retirement and a session loss keep it. A stop (also a user block
  close), the entry's removal and hub start clear it. Hub start clears every row before recovery.
- The admission: the selected run must come from the record's current entry, else `plugin <name>:
  no run is recorded; start it again`.
- The run check: the published entry exists and has the run's digest (the whole entry), the
  plugin directory exists, and the hub can start its sdk. It reads no lock. Else `plugin <name>:
  run <commit or digest> is no longer available; start it again`.
- A source run (section 4, "Source runs") has no published entry. The run check accepts it only
  when the hub runs with `-test` and `test_source_runs` still lists its dir. Its one other check:
  the dir and its main file exist. A run with no entry that is not a source run is refused with
  the text above.
- The selected graph uses only the depends of the selected runs. A cycle through the plugin
  refuses (`plugin <name>: dependency cycle among the selected runs (a -> b -> a); start it
  explicitly`). A dependency that no entry gives refuses (`plugin <name>: dependency <d> of run
  <commit> is not available`). A dependency that is not registered makes the plugin wait; for a
  refused, retained or stopped dependency the error is `waiting for <d> (<state>)`. A registered
  dependency is enough, whatever its pin is.
- A refusal gives state `refused`, so no crash backoff retries it.

**Repair entry point.** `hub.admin.start {"source": "<exact source string>"}` (beside
`{"plugin": name}`) resolves that entry again and starts it if the checks pass. Duplicate sources
are a malformed config, so a source names one entry. A hub restart works too.

**Run and candidate.** Each plugin has two records:

- `run`: the metadata of the current instance. The hub saves it in the plugins row (column `run`)
  at each spawn: source, dir, commit, digest, tag (a latest source only), events, notice, depends.
  Exports come from that instance's register. Notices use `run.notice`. Readiness is the
  registration of `run`. Watch roots come from `run` while an instance is recorded, else from the
  config entry.
- `candidate`: the last resolution attempt, `{commit, dir, tag}` or `{error}`. A resolution never
  writes into `run`. Only a spawn copies the candidate to `run`.
- `hub.plugins` reports state, `dir`, `commit` and `tag` from `run` (`commit` only for a GitHub
  source; `tag`, the release tag of the lock, only for a latest source, and no `tag` key for any
  other run), and `source`. A kept record also reports `dir`, `commit` and `tag` of its run. A run
  saved before the tag existed has no tag; a newer lock does not give it one. The `"candidate"`
  object:
  - A refused candidate: `{"error"}` only.
  - A passing candidate of an entry with a run: `"commit"` when the candidate commit differs from
    the run commit, and `"dir"` when the candidate dir differs from the run dir. Each field is
    independent. When run and candidate are both local, only `"dir"` can differ. A change between
    a GitHub and a local source reports `"commit"`, with `null` for a local candidate.
  - `"tag"` when the candidate tag differs from the run tag: the candidate tag, or `null` when
    the run has a tag and the candidate has none (for example a latest run whose source is now a
    branch). A tag change alone is a change. No `"tag"` key when the two are equal (both absent or
    the same tag).
  - No `"candidate"` key when no field differs, or when the entry has no run.
- `hub.plugins` reports the last resolution for `candidate`. After a lock write of an update, a
  resolution (for example `hub.plugins.reload` or an explicit start) gives the new candidate.
  `pinned_commit` reads the current lock: the commit that it names for a GitHub entry, or `""` (a
  local entry, or no pin). After an update's lock write and a crash, `commit` is the running commit
  and `pinned_commit` the new pin, until the next explicit start.
- A local run's `dir` is its snapshot, so a local candidate's source dir is not a `"dir"` change.
  A candidate of another source is: `"dir"` gives the new source dir.

Example: P runs C1. An update locks C2, and C2 fails its graph check. The hub keeps `run` = C1.
`hub.admin.start` on a running plugin changes nothing. (One exception: a local `expect_run` on a
running local plugin compares the snapshot digest of the current source, section 14.) To apply an
update, use `hub.admin.replace` (section 14), or `hub.admin.stop` and then `hub.admin.start`. In the example
the replace is refused and C1 runs on; after a stop, the start is refused, and the stop has
cleared `run` (no instance).

**`hub.plugins` fields.** `plugins` lists the running plugins with `source` and `dir`. `refused`
lists the refusals. `entries` has one item for each config and managed entry: `entry` (the
config index) or `managed` (the managed id), `source`, `name`, `state`, `dir`, `commit` and `tag`
(of the run), `pinned_commit` (of the current lock), `error` (state refused or retained), `waiting` (state waiting), and `"candidate"`
(above). An entry with a record adds `instance_id`,
`instance_pending`, `desired` and `registered` (section 14). `kept` lists each kept record:
`name`, `source`, `state`, `error`, and `dir`, `commit` and `tag` of its run.

**Retained instance.** Hub recovery with a refused candidate does not retire the recorded block.
The hub marks the row `retained`.

- `hub.register` from a retained instance fails with code `retained`: `instance <id> of <name> is
  retained after a refused start`.
- A retained instance satisfies no dependency. Its dependents wait, or are refused when the static
  check refuses them.
- `hub.admin.stop` retires a retained block.
- Source fact, not tested in a lab: Messaging, Profiles and `lazyrex.lua` read
  `rex.args.hub_client_id` at launch and register once at load. So an instance from an older hub
  calls the old hub client ID and cannot reach the new hub.

**Run.** The hub runs `rex do <dir>/<main>` (an SDK plugin: section 2, `sdk`). A changed
manifest name at a plugin start is refused: `the manifest name changed from X to Y; restart the
hub`.

## 8. Error texts

- `plugin github:lazy-rex/Messaging@main is not installed: the hub installs it at its next start
  or reload`
- `plugin messaging: the cache has no entry for commit <c>: the hub installs the locked commit
  again at its next start or reload`
- `plugin messaging: the cache entry for commit <c> changed (digest mismatch): the hub installs the
  locked commit again at its next start or reload`
- `plugin source ../Messaging: no lazyrex.json in <abs dir>`
- `plugin messaging: main ../x.lua must be a file inside the plugin directory`

Sections 3 to 7 give the other texts.

## 9. Harness (`lazyrex-hub test`)

- The test loader reads the manifest of the plugin under test and each `test.plugins` manifest. It
  applies the same name, main, dependency and cycle rules. A failure is a harness error (exit 2)
  with the same text.
- The generated lab config has local `{"source": "<abs dir>", "args": ...}` entries only, so the
  lab needs no lock. Its `test_source_runs` lists only the dir of the plugin under test (section
  4, "Source runs").
- `test.args` stays keyed by the manifest name.
- The harness adds the home plugin only when the plugin under test is not `lazyrex`. A
  `test.plugins` entry named `lazyrex` is a harness error. Any other duplicate name among
  `test.plugins` and the plugin under test is a harness error.

**Home plugin.** It lives in `plugins/lazyrex/`, with `plugins/lazyrex/lazyrex.json` (name
`lazyrex`, main `lazyrex.lua`) and its tests in `plugins/lazyrex/tests/`. The messaging stub is
`plugins/lazyrex/tests/stubs/messaging/`, with a manifest. Real configs list the home plugin as an
entry. The config field `home` names the plugin whose tabs mount first (default `lazyrex`).
`tests/harness-fail` and `tests/isolation` have their own directories, because main must stay
inside its directory.

## 10. Notice

The manifest owns the notice text (section 2, `notice`). There is no default text and no config
field. `storeBatch` checks every op before its transaction. A `notify` op from a plugin without a
notice fails with code `no_notice`: `plugin X has no notice in its manifest`. Nothing in that batch
is written. The notice of an instance is `run.notice` (section 7).

## 11. Lock reads in the hub

A resolution reads the lock and the cache with three functions. None of them runs git, uses the
network, or writes a lock file or a cache entry.

- `lockPath(cfgPath, field)` gives the lock file: the config field `lock` relative to the config
  directory, or the section 1 default. It reads no file.
- `readLock(path)` reads the lock file. A missing file gives an empty lock and no error: each
  GitHub entry is then "not installed" (section 8). A file that does not parse, or has another
  version, gives an error.
- `lockedDir(stateDir, lock, e)` checks the lock entry of the GitHub entry `e` and its
  cache entry: the key lookup, the agreement check (section 3), the cache entry, the manifest name
  and the digest (section 4). It returns the absolute plugin directory (the cache entry, or
  `<entry>/<subdir>`) and the locked commit.

Rules:

- Each resolution reads the lock once (`readLockSnapshot`), and calls `lockedDir` once for each
  GitHub entry with that one snapshot. When no entry is a GitHub source, the resolution reads no
  lock file.
- The error of `lockedDir` is the entry error, unchanged. It is a final text of section 3 or 8:
  no lock entry (`is not installed`), an agreement mismatch (`does not match: <field>`), an
  untrusted commit (`cannot be trusted`), no cache entry, a changed cache entry (`digest
  mismatch`), and a cache entry that cannot be read (`plugin <name>: cannot read the cache entry
  <absolute dir>: <error>`).
- A `readLock` error refuses every GitHub entry. The hub start does not fail (section 1: only a
  malformed config fails the start), and local entries resolve as usual. The graph rules refuse a
  local dependent of a refused GitHub entry. The ownership and retained-instance rules apply. The
  text depends on the error:
  - The file does not parse, or has another version: `plugin <source>: the lock file <path> is
    not valid: <error>; repair the file or restore it from a backup`. The lock writers refuse it
    too, and never replace it: a lock that does not decode is never taken as an empty lock.
  - A filesystem error (for example, no read permission): `plugin <source>: cannot read the lock
    file <path>: <error>`.
- After `lockedDir`, the hub reads the manifest with `readPluginManifest(dir)`, as for a local
  source, so the manifest rules and the graph are the same for both kinds of source.
- A hub start takes neither plugin command flock (section 6). The hub reads the lock with one
  read. The plugin manager replaces the file with a rename, so the hub reads the old lock or the
  new lock.
- Tests: the `remote` option of `pluginCmdOpts` gives the fetch URL, so a unit test can use a local
  bare repository (`file://`) in place of GitHub. No CLI flag, config field or environment variable
  sets it. A lab hub reaches its test repositories through `url.insteadOf` in the lab's own git
  config, and installs its managed entries at its start.

## 12. Tests

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
| An installed GitHub source (also a subdir source), with local entries and dependencies across both kinds | resolves from the read-only cache entry; the run has the locked commit | U |
| A lock that does not parse; a lock that cannot be read (a directory, no permission) | each GitHub entry refused with the exact text; local entries resolve | U |
| An update op, then a new resolution | the run keeps its commit, also after a save and a load; the candidate has the new commit and dir | U, L |
| A pin document: each malformed form; an empty document round trip | refused before any fetch; `"entries": []` | U |
| export: each GitHub entry passes; one fails; a static refusal; a malformed lock | the document; nothing written and no identity | U |
| `hub.plugins` candidate fields | `{"error"}` when refused; each differing field; no key when none differs or without a run | U |
| A latest run in `hub.plugins` | `tag` in the entry and in a kept record; no `tag` key for another run; candidate `"tag"`: the new tag, or `null` for a candidate without one; no key when equal; a saved run keeps its tag across a load | U |
| A latest run in a hub: a new release, update, reload, then replace with the old and the new `expect_run` | the candidate has the new commit and tag, the instance is the same; `candidate_changed`; a new instance with the new tag and no candidate | L (`TestLatestLab`) |
| A release tag: each form; the order (`v1.10.0` > `v1.9.0`, no length limit) | only `vX.Y.Z` without a leading zero; decimal order | U |
| @latest resolution: higher tags to a tree or a blob; no release; a failed fetch or ls-remote; a tag that moves before the fetch | skipped and named; refused, no branch fallback; the resolution fails, no older release; refused | U |
| The install of a latest source: first; no release; a lock entry with no tag | the highest release; refused; refused | U (`TestLatestInstall`, `TestLatestInstallNoRelease`, `TestLatestInstallRefusesMissingTag`) |
| The lock and pin document versions | version 2 only with a latest form, else the bytes before `@latest`; a version 1 file with a latest form refused; back to version 1 when the last latest entry goes | U |
| A GitHub run with a local candidate; a local run with a GitHub candidate | `"commit": null` and the dir; the locked commit and the dir, with no top-level commit | U |
| An install of a new entry; a missing cache; an update that moves a branch | the lock gets the entry; a new dir at the locked commit; a new commit in the lock | U (`TestCmdEnvInstallAndUpdate`), L (`TestPluginInstallCacheLab`) |
| A suffix-only ref match; branch and tag; annotated tag through the real fetch path; invalid ref; 40-hex that is not a commit; unavailable commit | refused, or peeled as specified | U |
| An executable helper; a symlink; a submodule in the tree | kept 0555 and the digest passes; refused; refused | U |
| A lock agreement mismatch (each field) | refused; the text names the field | U |
| An untrusted locked commit; then an update | refused with the update text; the entry resolves | U (`TestLockAgreement`, `TestUpdateClearsLockMismatch`) |
| Two flock holders on one state with two lock files | the second fails at once | U (`TestFlocks`) |
| A start from a read-only cache entry that loads a sibling module (`require`) | runs | L |
| `hub.admin.start {source}` after a repair | the entry resolves and starts | H |
| A notify from a plugin without a notice | `no_notice`; no record written | U, H |
| Harness: the home plugin under test; a `test.plugins` duplicate or `lazyrex` | one home plugin; a harness error | U |

## 13. Managed plugins

The plugin manager adds and removes plugins without editing the user's config. It writes the
managed file. The hub reads the managed file with the config.

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
- `watch_roots`: as in a config entry: an array of absolute, clean paths.
- Unknown fields are refused. This rule also keeps the fields `user_messaging` and `prompts` out of
  the managed file: only the user's config can set them. They are hub config fields (docs/hub.md,
  "Agent prompts and acting as the user").

**Missing and malformed file.**

- A missing file is an empty list. The manager may never have written it. Only a file with an
  empty `plugins` array is another empty list.
- A malformed file: bad JSON, a root that is not an object with a `plugins` array (also `null`,
  `{}` and `{"plugins": null}`), an unknown field, a bad `id` or source, two entries with the same
  `id`, two entries with the same source, or a file over 1 MiB (1048576 bytes,
  `managedFileLimit`; the plugin manager refuses to write a larger file).
  - Hub start does not fail. The hub starts the config plugins and refuses every managed entry.
    `hub.plugins` reports the file error once, as `{"managed_file": "<path>", "error": "..."}`.
    Reason: a defect of the manager must not stop the plugins that the user configured.
  - The plugin manager's ops on managed entries fail, and the lock stays unchanged.

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

**Reload.** `hub.plugins.reload` reads the managed file again while the hub runs. Config entries
do not reload: a config change needs a hub restart.

- Callers: an operator (a process outside agent and plugin panes), or the registered current
  instance of a plugin that has an entry. The hub checks a plugin caller again after it takes the
  reload lock. Other callers get code `retained` (a kept instance) or a refusal.
- A malformed file changes no entry and no instance. The reply is an error that ends with `; the
  managed entries are unchanged`, and `hub.plugins` reports the file error in `refused`.
- A valid file, by managed id:
  - unchanged (the same source, args and watch roots): the entry stays;
  - changed: the hub updates the entry and keeps its bound name. A running instance runs on.
    `hub.plugins` reports `"candidate": {"dir"}` while the new source differs from the run's
    source (a GitHub run: while the new dir differs from the run's dir).
    `hub.admin.stop`, then `hub.admin.start`, runs the new source;
  - new: the hub adds the entry and starts it;
  - missing: the record becomes a kept record (2d part 1). Its instance runs on with its
    registration and exports until `hub.admin.stop`. It gets no notices and never respawns. A new
    `hub.register` for it is refused (code `retained`). A dependent's next start is refused
    (`dependency <name> is not configured`).
- Re-add: an entry with the same name and source adopts a kept record. The re-add start runs the
  start check first:
  - a pass with a registered live instance: the instance stays, and its state is clear again;
  - a pass with no such instance: the hub retires the recorded block and spawns;
  - a fail: the instance stays refused or retained. The reply reports the error.
  A re-add does not restart a dependent that a removal refused. `hub.admin.start` starts it.
- `hub.admin.start` of a retained instance runs the start check again, by the re-add rule.
- Admission: a start reads its entry and writes its state under one lock section. A reload swaps
  the entries under the same locks. A start that sees the removal spawns nothing. A start that
  was admitted before the removal completes, and its instance is kept.
- Lock order: the reload lock, then a plugin's lifecycle lock, then the entry lock, then the hub
  lock. Hub start recovery holds the reload lock for its whole run.
- The reply: `{added, changed, removed, readded, errors}`. `errors` maps a plugin name to the
  error of its start.
- The reload runs its starts before it replies, as `hub.admin.start` does. It does not wait for a
  registration. Rex gives the hub one request at a time, so a reload that waits on a start's
  lifecycle lock holds the hub's other requests for that time. A caller timeout does not roll
  back a reload: the entries are already applied. Read `hub.plugins` to see the result.
- Events: `plugins_reloaded {added, changed, removed}` and `plugins_reload_refused {error}`.

A reload keeps `run` and unresolved spawn intents when an entry changes or disappears.

**Orphaned lock entries.** A lock entry whose source is in neither the config nor the managed
file never starts a plugin. For example, a cancelled add of the manager can leave one. A manager op
removes the lock entry of the source that it stopped naming (update, remove). A lock entry that no
entry names stays; it is not used.

**Size bounds.** The hub refuses a `lazyrex.json` over 64 KiB (`maxManifestBytes`, `sources.go`),
for config and managed entries and for the harness. The text: `<dir>/lazyrex.json is <n> bytes,
over the 65536-byte limit`.

- The read itself is bounded: the loader reads at most 65537 bytes. A length check after an
  unbounded read would not limit the memory.
- Tests: a manifest of exactly 65536 bytes is accepted; 65537 bytes is refused.
- Main file: no hub bound. The hub does not read the main file: it checks its type, and `rex do`
  reads it. (The digest reads every file, as a stream.) Whether Rex limits the size of a `rex do`
  script: not tested.

## 14. Replace (`hub.admin.replace`)

`hub.admin.replace {"plugin": "<name>", "expect_instance": "<instance id>"}` replaces the running
instance of an entry-backed plugin with a new instance from its current candidate. It is an admin
action: the guard of the other admin actions applies (an operator only). Use it after
a lock write of an update, or after a local source changed. `expect_instance` is the
`instance_id` that `hub.plugins` reports. A replay, or a race with another change, then cannot
replace another instance.

Arguments: both fields are required and nonempty. `expect_run` is optional (below). Other fields
are refused (`invalid`).

**expect_run.** `hub.admin.replace` and `hub.admin.start` take an optional
`"expect_run": {"source": "<exact source>", "commit": "<40 hex>", "digest": "sha256:<64 hex>"}`.
commit and digest are required for a GitHub source and refused for a local source (`invalid`). The
source string binds owner, repo, subdir and ref, so the same commit in another subdir is another
run. `hub.plugins` gives each entry's candidate as `expect_run` (docs/hub.md), so a caller passes the read value unchanged. A writer of the lock (plugin use, the plugin manager) names the run that it wrote; a pin that
another writer wrote later is then refused, not started.

- replace: after the candidate check (step 7), the candidate must be the expected run, else
  `candidate_changed`, "plugin <p>: the candidate is <run>, not <expected>; the running instance
  <id> is unchanged; nothing was retired or spawned".
- start of a stopped plugin: the start path runs its resolution; the candidate must be the expected
  run (else `candidate_changed`, "...; nothing was retired or spawned"), and each dependency must be
  registered (else `dependency_not_ready`). Both checks are one final admission, after the
  resolution and before any retirement; in the same `h.mu` section the record becomes desired
  running, and a failed save refuses. Then the hub retires the recorded instance and spawns from
  that candidate, as a replace does. A refusal leaves the desired state as it was. The start never
  enters `waiting`, also when a dependency stops after the admission, because a later wake would
  start the candidate of that time. A retained live instance is kept only when its run is the
  expected run. An entry that a reload removed after the resolution refuses the start (code
  `refused`), and the kept record gets no desired state. (The hub serves one request at a
  time, so a reload cannot come inside a start; the check guards a concurrent writer.)
- start of a running plugin: its run must be the expected run, else `candidate_changed`, "plugin
  <p> runs <run>, not <expected>; nothing was started". A run saved before the digest field has no
  digest, so it does not match a GitHub expectation: replace it.
- start of a running local plugin (a local `expect_run`, the lead's ruling of 2026-10-22): the
  expectation names the source, and a local run is identified by its snapshot digest. So the
  start makes the snapshot of the current source (section 4, the same bounds), under the start
  lease:
  - the digest of the running run: the instance is kept (no restart);
  - another digest: the start replaces the instance with the new snapshot, as a start of a
    stopped plugin does, and the selected run names the new snapshot. A later crash respawn runs
    it;
  - a failed snapshot, or a candidate that does not pass its check: code `refused`, "...; the
    running instance <id> is unchanged". The instance, its state and its selected run do not
    change.
  A plain `hub.admin.start` (no `expect_run`) of a running plugin changes nothing, for both
  source kinds.
- The candidate and the run carry the digest of the lock entry (`digest` in the run JSON).

Tests: `replace_test.go` (TestExpectRunArguments) and `expectrun_lab_test.go` (local sources:
replace and stopped start, refused and accepted, a running start, `dependency_not_ready`;
`selectedrun_lab_test.go` TestSelectedRunLocalExpectRunLab: a running local start with an
unchanged source, a plain start, a failed snapshot, a candidate that does not read, and the new
digest with a crash after it; a GitHub
source: the same commit in another subdir, on replace and on a stopped start; a dependency that
stops between the resolution and the final admission: `dependency_not_ready`, and no later start).

Locks: `reloadMu`, then the plugin's lifecycle lock, then `entriesMu` only to resolve. A reload
cannot swap the entries during a replace, so the candidate and the entry membership stay stable
from the check to the spawn. A crash respawn, the waiting wake, a stop and a block close take the
lifecycle lock, so they come before or after the whole replace.

The order. The first failure ends the replace with its code:

1. Load the plugins row.
2. Resolve the spawn intents (section 7). An unresolved intent: `unresolved_intent`. The
   reconciliation can adopt a found block or abandon an intent before a later refusal.
3. The desired state is `running`, else `stopped`.
4. An owned live instance, else `no_instance`. The owned instance is the recorded instance while
   it has a block, else the instance of an open spawn intent.
5. The owned instance is `expect_instance`, else `instance_changed`. The text names the owned
   instance.
6. The record has an entry, else `refused` (a kept record: stop it with `hub.admin.stop`).
7. The candidate check (`validateCandidate`): a resolution, with no write of state, refusal,
   waiting, client, exports, run or ownership. A refused candidate: `refused`, "<error>; the
   running instance <id> is unchanged". The old instance stays registered and ready; a running
   dependent is not changed. A new start of a dependent can still be refused by the graph rule.
8. Each dependency of the candidate is registered, else `dependency_not_ready`.
9. Retire the old instance (`retire`). An error, or a block that holds a process that the hub
   cannot verify: `retire_failed`, "nothing was spawned". A block that is gone from its live
   session: the plugin is stopped (as a native close), code `stopped`.
10. Spawn from the candidate (`spawn`).

Until step 9 begins, a refusal retires nothing and spawns nothing. After step 9 begins, the old
instance can be gone or partly stopped.

The spawn outcome:

- Success: Rex returned a block ID, and the hub read its child pid. The event
  `plugin_replaced {plugin, previous_instance, instance_id, run: {source, dir, commit}}`, and the
  reply has the same fields (`commit` only for a GitHub source). Success means that this request
  created the reported instance. It does not mean that the instance registered or works.
- A known failure, with nothing created: `spawn_failed`. The text names the stage: no work
  session, the spawn intent was not recorded, the spawn intent was not marked as sending, or Rex
  refused the new block. The old instance is retired, and the plugin has no instance; start it
  with `hub.admin.start`.
- A known failure whose intent stays open: `spawn_failed`. The hub abandons the intent after a
  known failure, and checks the update. If the update fails, the text names the intent and its
  status, and `hub.plugins` reports it as pending. A `prepared` intent was never sent:
  `hub.admin.start` resolves it. A `sending` intent (Rex refused the block) stays unresolved:
  `hub.admin.start` gets `unresolved_intent` until the intent's session closes.
- An unknown outcome (a timeout, a disconnect, no block ID, or a failed read of the child pid):
  `spawn_unknown`. The intent stays open, and `hub.plugins` reports it. Do not retry: a later
  replace gets `unresolved_intent` while the intent is unresolved, so no second instance can
  start. When the reconciliation finds the block, it adopts it, and a replace with the old
  `expect_instance` gets `instance_changed`.

`hub.plugins` entries with a record add `instance_id` (the owned instance, or ""),
`instance_pending` (true when the owned instance is only an open intent), `desired` and
`registered`.

Tests: `replace_test.go` (the arguments, the local spawn stages) and `replace_lab_test.go` (a
replace and its replay, Stop before and after, a refused candidate, `dependency_not_ready`, the
spawn outcomes with the `plugin-create` faults, `retire_failed`, and a reload after a replace).

## 15. Limits

- `depends` has no consumer of files. A plugin cannot read its dependency's directory through the
  hub.

# Release sets

A release set is a group of plugin releases that follow one hub release, in live order. The release
tool rehearses the hub release and all plugin steps together in one lab. Then each live plugin step
must pass a gate: the live state before the step must equal the state that the lab saw before the
same step.

This document is for the operator of a release and for the plugin owners who review set entries.
The code is in `hub/cmd/hub/releaseset*.go`, `releasesnap.go` and `releaserehearse.go`.

## The command sequence

1. Write the set file (see "The set file"). Put the entries in live order. Leave out `review`.
2. Run `lazyrex-hub release set-hash -set <file>`. For each entry, it prints this line, then the
   canonical bytes on the next line:

   ```text
   plugin <name> entry_sha256 <hash>
   ```

   set-hash reads only the file. It reports an entry that breaks a rule. It does not report a missing
   or stale review.
3. Send each plugin owner the canonical bytes and the `entry_sha256` of the owner's entry. The owner
   reviews those exact bytes.
4. Add the owner's review to the entry: `{"by", "msg_id", "entry_sha256", "from", "to"}`. `from` and
   `to` must equal the entry's commits. `entry_sha256` must equal the hash from step 2.
5. Freeze the live hub run as for any hub release. By default, H must be on `main` of origin.
   When H is on a release branch instead, name it: `lazyrex-hub release freeze -spec <file> -run
   <dir> -branch release/<major>.<minor>`. The name must have exactly that form. The freeze fetches
   that branch by its full name, so a stale remote-tracking ref or a fetch config with only `main`
   cannot answer for it. It refuses H that is not on the named branch, and it does not try `main`
   then. The candidate records the branch, and the lab freeze of the rehearsal uses the same
   branch. A plugin freeze refuses `-branch`.
6. Rehearse the hub run with the set:

   ```sh
   lazyrex-hub release rehearse -run <hub run> -lab /tmp/rexlab-release-<name> -set <file>
   ```

   The tool copies the set file into the hub run as `set.json`, and the record binds its sha256.
   A later rehearse of that run without `-set` is refused while `set.json` is there.
7. Take the sha256 of `<hub run>/rehearsal.json`. This is the `record_sha256`.
8. Run the live hub release.
9. For each plugin, in set order: write a live plugin spec with this field, freeze, and run the
   steps. The precheck runs the set gate.

   ```json
   "set": {"run": "<absolute path of the hub run>", "record_sha256": "<64 lowercase hex>"}
   ```

   A lab spec cannot have `set`.

## The set file

The file is one JSON object: `{"plugins": [<entry>, ...]}`. Unknown fields are an error. The list
must not be empty. A plugin must not occur twice.

An entry has these fields:

| Field | Content |
|---|---|
| `plugin` | The plugin name (`^[a-z][a-z0-9_]*$`). |
| `from`, `to` | 40 lowercase hex commits. They must differ. FROM must be the live lock pin. |
| `seed` | Items that write lab data with FROM. No `sql` item. |
| `probe` | Items with expected values. At least one item must be `action` or `view`, and each of those needs `expect`. |
| `keys` | For each table, the columns that identify a row. Not `rowid`. Not a `lazyrex_*` table. |
| `changes` | Each changed value that is allowed (see "The data rules"). |
| `inserts` | For each table, a `max` of new rows and `invariants`. Each invariant is a SELECT that must return no row. |
| `review` | The owner review (step 4). |

There are three kinds of item:

- `action`: the lab agent calls the plugin action `action` with `args` (a JSON object). The action
  must not be a `hub.*` action. The item has only `kind`, `action`, `args`, `expect` and `until`.
- `view`: the user path. A lab pane in `pane_dir` (must start with `<LAB>/`) runs the palette
  `command`. A lab renderer attaches the view at `route` and sends the node action `action` on
  `node_id`, with `input` and `operation_key`. The lab renderer is the test renderer of the tool
  (`testview`), not the renderer of the candidate. Each hub start (`dev.sh`) builds the candidate
  renderer at the config `view` path, so the tool writes its wrapper there again before each
  view item. Each `operation_key` must be unique in the file. The
  tool replays each view operation with the same key. The replay must give no data change.
- `sql` (probe only): a read-only SELECT on the lab plugin database, with `params`. `rows` must be
  exactly equal. Use `[]` for no row.

The order of the items in `seed` or `probe` is not always the run order:

1. First, all `action` items run, in their written order, in one lab pane.
2. Then the `view` and `sql` items run, in their written order.

So an action item that comes after a view item in the list runs before that view item. If an item
needs data from another item, put both in the order above. Owners must check this when they review
the dependencies of seed and probe items.

The tool uses `operation_key` as written for a seed item. For a probe item, it appends the phase
(`<operation_key>-<phase>`), so each probe run has its own key. The replay of a view operation uses
the same derived key.

An `expect` is a list of `{"pointer": <JSON pointer>, "value": <JSON>}` or
`{"pointer": ..., "empty": true}`. `value` is an exact comparison of canonical JSON. `empty` accepts
`{}` and `[]`, because Lua encodes an empty table as either.

### Until

An action item can have `until`: a list with the same form as `expect`. Only an action item can
have it. The runner repeats the call until the reply meets every `until` entry:

- Each attempt runs in the same lab pane, as the same agent, with the same args file. So a keyed
  action is a replay of the same operation.
- One window of 60 s covers all attempts of the item, the calls and the 1 s pause between them. It
  starts when the pane starts the first attempt. A call does not get its own 60 s: each call gets
  only the time that is left.
- A nonzero exit is a reply that does not meet `until`. The runner repeats the call while the
  window has time. For example, a plugin can answer `unavailable` for a short time after it
  starts. A replay with the same key is safe. When the window ends, the error gives the last exit
  code and reply.
- To wait for a state, put `until` on a read-only status action, and put the next write after it.
- A reply that comes after the window fails the item, even when it meets `until`.
- When the window ends while an attempt runs, the item fails at once. The pane stops after that
  call. The error gives the last completed reply and its exit code, or says that no attempt
  completed.
- The tool logs each attempt: the attempt number, the exit code and the reply.
- The later items of the batch run only after the `until` item is met.

`expect` is checked once, on the reply that met `until`, in the phases that check expects (not in
the seed). An item without `until` runs once, as before. The owner review covers `until`, because
it is part of the entry hash.

### The entry hash

`entry_sha256` is the sha256 of the canonical JSON of the entry without `review`. The canonical form
has sorted keys, no spaces, numbers as written, and no HTML escapes (the form of `jq -cS`). A change
in any field except `review` changes the hash, and the review is then stale.

The tool refuses JSON in which one object has two members with the same name. A decoder keeps only
one of them, so the reviewed bytes would not show the value that runs.

### Lab placeholders

Strings in `args`, `input`, `expect` and `until` values can contain these placeholders. The tool
replaces them in the lab only. The reviewed bytes keep the placeholders.

| Placeholder | Lab value |
|---|---|
| `<LAB>` | The lab directory. |
| `<SELF>` | `relseed`, the lab agent that runs the action items. |
| `<PEER>` | `relpeer`, a second lab agent that never joins. |
| `<SESSION_ID>`, `<BLOCK_ID>` | The session and block of the current pane: the pane of the agent batch for an action item, the source pane for a view item. |
| `<SEED_SESSION_ID>`, `<SEED_BLOCK_ID>` | The session and block of the plugin's seed action batch. The seed batch records them before it fills its own args. An item that names them before that batch, or in a plugin with no seed action item, is refused. |

Each agent batch runs in a new lab session with one tab and one pane. The pane directory is the
batch directory under `<LAB>`, and the pane reports it (OSC 7) before the first call. The tool
destroys the session after the batch.

Each view item runs the same way: its source pane is the one pane of a new lab session, in
`pane_dir`. The window shows the pane, because the hub refuses a palette command from a block that
no window shows. The pane reports `pane_dir` (OSC 7) before the palette command. The tool destroys
the session after the item.

So a probe that replays a keyed seed action must name the seed pane with the seed placeholders.
Then the args are the same as in the seed. The seed session no longer exists in the probe, so the
replay must return the recorded result. A new operation on the seed session fails.

If two member names become one name after the replacement, the tool refuses the item.

### Lab files

The set rehearsal adds these to the lab files only. It refuses a config or profiles file outside the
lab, and a live config or profiles file that already has one of these ids.

| Item | Lab value |
|---|---|
| Agents `relseed`, `relpeer` | Roster agents of the lab config. |
| Profile `lab_inert` | Kind `accessory`, command `["true"]` (for example the Profiles view path). |
| Profile `lab_agent` | Kind `agent`, command `["true"]` (for example an automation, which needs an agent profile). |
| `<LAB>/repos/demo` | A Git repository with one empty commit on `main` and no remote. |
| `<LAB>/src` | An empty directory, for example a `pane_dir`. |

Do not name a live profile (for example an agent profile of the user) in a set entry. A lab process
must not start a real agent.

## The data rules

All seed data is made in the lab by the FROM plugin code, through its own actions or views. Do not
copy live data, a live `hub.db`, or ownership rows into a set file or a fixture. The rehearsal reads
the live files only. It writes only its record files into the live hub run.

After the seed, the tool takes a reference snapshot of each plugin database. It compares later
snapshots with the reference:

- The schema (every `sqlite_master` row except `lazyrex_*` and `sqlite_stat*`) and `user_version` must
  be exactly equal. 4.2 has no schema change.
- A lost table, column or row fails.
- A changed value needs a `changes` entry for that row (by `key`) and `column`. Its `expect` is the
  exact new value, or `{"sql": "<boolean expression over old and new>"}`.
- A new row needs an `inserts` entry for its table. The rows added since the reference count
  against `max`.
- A rerun of replace and verify must change no data, compared with a snapshot taken right before
  the rerun.

## What the rehearsal does

The set phase runs after the hub forward and before the hub rollback:

1. A chunking probe: a lab plugin `sp` sends a large reply in parts. The tool checks the hub log
   window of the probe.
2. The seed of each plugin, the reference snapshots, and the probes on FROM.
3. Two fault cases on the first plugin, both at FROM:
   - failed-forward: a lab-mirror commit on top of TO whose main file fails at load. Its forward
     must fail at replace or verify. Then the rollback, the data checks and the probes on FROM.
   - interrupted-replace: the real TO. The replace stops after the hub call (exit 3). The journal
     must have an open begin, and the hub must run TO. Then a rerun of replace and verify, the
     rollback, and the probes on FROM.
4. The forward of each plugin, in set order: a lab plugin run with freeze, precheck, backup, pull,
   replace and verify, with data checks. Then a rerun that must change nothing, and the probes on TO.
   The tool records the frozen candidate and the base pins of each step.
5. A second chunking probe.
6. The rollback of each plugin, in reverse order, with data checks and the probes on FROM.

The record (`rehearsal.json`) has a pass only with one step per plugin, two chunking windows and two
fault cases. The fault cases are evidence of recovery only. The gate does not read them.

## The live gate

The precheck of a live plugin spec with `set` reads the record of the hub run. It refuses the step
if one of these is not true:

- The record has the `record_sha256`, a pass, two chunking windows and the frozen steps.
- The tool binary, `set.json`, the live config and the live managed file are the bound ones.
- This plugin is in the set. Its FROM and TO equal the entry and the frozen candidate.
- The frozen candidate and the base pins equal the rehearsed step: the source, the name, FROM, TO,
  the peer, the pins, the lock pins, the hub commit, the SDK digests, the reasons and the base pins.
  So every earlier step is released, and no later step.
- The hub run is the live hub release of the same config and server. It released the running hub
  commit. It has no rollback. Its last journal record is a passed verify.
- The running hub is the H hub that the hub run started.
- The running hub loads the SDK from `<release_dir>/LazyRex/sdk/lua`, and the bytes of that
  directory have the rehearsed digest.

## Proof limits

- The lab test `TestReleaseRehearseLabSet` uses four plugins (Tunnels, Messaging, Templates, then
  Profiles) with action, sql and view items. Templates and Profiles use the exact items of their
  4.2 kit entries (`TestReleaseSetLabKitEntries` checks the approved entry hashes). The Templates
  probes replay keyed seed actions with the seed placeholders, after the seed session is
  destroyed. The Profiles items are view items: the user path through `hub.palette.run` from a
  source pane.
- The lab test proves lab-to-live equivalence by a second rehearsal in another lab directory (lab
  B). Each step of lab B must equal the step of the first record under the gate's own comparison,
  and the SDK paths must differ. This proves freeze equivalence and the gate unit tests. It is not a
  run of the live gate from the CLI.
- The fault cases run only for the first plugin of the set.
- The lab test runs a real `until` item (Messaging history) through the lab pane, but its reply meets
  `until` at the first attempt. The repeats, the shared window and the timeout are proven by
  `TestReleaseSetUntilBatch`: it runs the real pane script under `/bin/sh` with a stand-in `rex`.
- The release labs run on Linux. The final 4.2 rehearsal runs on the Mac under the load gate.

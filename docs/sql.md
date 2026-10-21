# Plugin SQL databases (hub.sql)

Status: **proposed, draft 1** (2026-10-21). Nothing here is implemented. The user decided on
2026-10-21 that each plugin gets its own SQLite database, run by the hub, through hub actions.
That decision replaces the part of "LazyRex hub services own namespaced storage and avoid
synchronous call cycles" that said raw SQL is not exposed. Its other rules stay: each plugin has
its own storage, and no call cycle is synchronous.

Labels: "observed" (with the Rex or library version), "untested", "proposed".

## 1. The database

- Each plugin name has one SQLite file: `<state>/sql/<name>.db`, with its `-wal` and `-shm` files.
  The plugin name is the identity that already keys the key-value namespace (`records.ns`) and
  the `plugins` table. A replace, a prune, a source change and a hub restart keep the file. The hub
  never deletes it. An operator deletes it by hand, with the plugin stopped.
- The hub opens the file at the first `hub.sql.*` call of the plugin. Settings, fixed by the hub:
  WAL journal, `synchronous = FULL`, `foreign_keys = ON`, `trusted_schema = OFF`, SQLite defensive
  mode, and no attached databases (`SQLITE_LIMIT_ATTACHED = 0`). The plugin cannot change them.
- The file size limit is 1 GiB (`max_page_count`). A write over it fails with `too_large`.
- The plugin owns its schema and its migrations. It can read and write `PRAGMA user_version`. The
  hub adds no migration help.
- Names that start with `lazyrex_` are the hub's. The plugin cannot create, read, write or drop
  them (section 5).

## 2. Who may call, and order

- Only the registered current instance of the plugin may call `hub.sql.*`, for its own file. The
  check is the one of `hub.store.*`. A retired instance gets `stale_instance`.
- The hub runs the calls of one plugin one at a time, in arrival order, on one connection. Calls of
  two plugins run in parallel. Reason: a plugin never gets `busy` from its own calls, and each call
  sees every commit before it.
- Fencing: a replace or a stop retires the old instance first, and then waits until that plugin's
  running SQL call (if any) has ended. After the retirement returns, no statement of the old
  instance runs. Calls that wait in the queue are checked again when they start.
- More than 64 waiting calls for one plugin: `busy`. The plugin tries again later.

## 3. Actions

All three take `sql` (one statement; text after the first statement is refused) and `params`.

### hub.sql.exec {sql, params?}

Runs one statement in its own transaction. Reply: `{changes, last_insert_rowid}`.

### hub.sql.query {sql, params?, max_bytes?}

Runs one read statement in one read transaction (one snapshot). Reply:
`{columns: [names], rows: [[values]], complete}`.

- The hub adds whole rows until the next row would make the encoded reply larger than `max_bytes`
  (default and maximum: the reply limit, section 4). Then `complete` is false, and the reply has the
  rows that fit.
- If the first row alone is too large: the error `too_large`, with `{row: 0, bytes}`. A row never
  disappears without an error.
- The hub keeps no cursor. To continue, the plugin queries again with keyset paging (for example
  `WHERE seq > ? ORDER BY seq LIMIT ?`). So there is no cursor lifetime and nothing to lose at a
  restart.
- A statement that writes is refused (`invalid_input`). Use `hub.sql.exec` or `hub.sql.batch`.

### hub.sql.batch {statements, key?, notices?}

Runs a list of 1 to 64 statements in one transaction (`BEGIN IMMEDIATE`). Each statement:
`{sql, params?, query?, expect_changes?}`.

- `query: true`: a read statement; its result is in the reply, `{columns, rows}`. All its rows must
  fit, else the batch rolls back with `too_large`. Reads in one batch see one snapshot, and also
  see the writes of the earlier statements of the batch.
- `expect_changes: n`: after the statement, the hub compares `changes` with n. A difference rolls the
  batch back with `conflict` and `{index, changes}`. This is the conditional write.
- Any error rolls the whole batch back. The error has the statement `index`.
- Reply: `{results: [...]}`, one entry for each statement, in order: `{changes, last_insert_rowid}` or
  `{columns, rows}`.
- `key` (1 to 128 bytes): the hub saves the reply in `lazyrex_ops` in the same transaction. A batch
  with a saved key and the same request answers the saved reply and runs nothing. The same key with
  another request: `operation_conflict`. So a lost reply or a timeout is safe to retry with the same
  key. The hub keeps a saved reply 7 days, and at most 10 000 keys for each plugin, oldest first
  (proposed numbers).
- `notices` (section 7).

## 4. Values and limits

**Parameters.** `params` is a JSON list (positional `?`) or an object (named `:name`). Values:

| JSON | SQLite |
|---|---|
| `null` | NULL |
| `true`, `false` | INTEGER 1, 0 |
| an integer number, at most 2^53 in size | INTEGER |
| `{"int": "<decimal>"}` | INTEGER, any signed 64-bit value, exact |
| a number with a fraction or exponent | REAL |
| a string | TEXT |
| `{"blob": "<base64>"}` | BLOB |

Anything else: `invalid_input`.

**Results.** INTEGER: a JSON integer, exact (Go writes int64 exactly). REAL: a JSON number; an
infinite value is the error `unsupported_value`. TEXT: a JSON string; text that is not valid UTF-8
comes back as `{"blob": "<base64>"}`. BLOB: `{"blob": "<base64>"}`.

Open fact: a JSON integer above 2^53 is exact only if Rex Lua decodes it into a 64-bit Lua integer.
That is **untested**. I check it on a lab server before the implementation, and record the
`rex_version`. If Rex Lua decodes it into a float, INTEGER results above 2^53 come back as
`{"int": "<decimal>"}` instead.

**Limits.**

- Request: the plugin's whole `client.perform` message is at most 64 KiB (observed on Rex
  `0.0.1792158932-g88fd41`, rex-protocol.md). The hub adds: `sql` at most 32 KiB for each
  statement, at most 999 parameters for each statement.
- Reply: the hub's reply guard limit (1 047 552 bytes, observed with the same Rex build).
- Time: 5 s for an exec or a query, 10 s for a whole batch. At the limit the hub interrupts the
  statement, rolls the transaction back, and only then replies `timeout`. So a `timeout` reply means
  "nothing was applied, and nothing still runs".
- The caller's own Rex timeout is another case: the hub may still commit after the caller stopped
  waiting. The outcome is then unknown to the plugin. Use `key` and retry.

**Error codes.** `invalid_input` (bad arguments, more than one statement, a parse error, a write in
`hub.sql.query`), `refused` (section 5), `sql_error` (with SQLite's extended result code name, for
example `SQLITE_CONSTRAINT_UNIQUE`, and its message), `conflict`, `operation_conflict`, `too_large`,
`timeout`, `busy`, `stale_instance`, `unsupported_value`.

## 5. What a statement may do

The hub installs a SQLite authorizer on each connection. SQLite asks it for each action of a
statement when it prepares the statement, also for the statements of triggers and views.

- Allowed: SELECT, INSERT, UPDATE, DELETE, with every built-in function (JSON1 too); CREATE, DROP and
  ALTER of tables, indexes, views and triggers; `ON CONFLICT ... DO UPDATE`; partial indexes;
  `WITHOUT ROWID`; temporary tables.
- PRAGMA: read `user_version`, `foreign_keys`, `table_info`, `table_xinfo`, `index_list`,
  `index_info`, `foreign_key_list`, `foreign_key_check`, `integrity_check`, `quick_check`; write
  `user_version`. Every other PRAGMA: `refused`.
- Refused: ATTACH, DETACH, VACUUM (also VACUUM INTO), `load_extension`, CREATE VIRTUAL TABLE,
  BEGIN, COMMIT, ROLLBACK, SAVEPOINT and RELEASE (the hub owns the transactions), and any access to
  a `lazyrex_` or `sqlite_` table (defensive mode also protects the schema).

**Library fact.** modernc.org/sqlite v1.60.1, the hub's driver today, has no authorizer and no
limit API. v1.61.0 adds `RegisterAuthorizer` and `Limit` (read in the module source,
`authorizer.go` and `sqlite.go`). v1.61.0 was published 2026-10-21 11:57 UTC (the Go module
proxy). This is a version change of an existing dependency, not a new one. Coordinator: decide
whether the implementation may use a version that is this new, or waits some days for it. Without
an authorizer, the only option is a text check of each statement, which I do not propose: a text
check misses trigger and view bodies.

## 6. Render dependencies

- A view can declare `deps.tables`: a list of table names. A committed `hub.sql.exec` or
  `hub.sql.batch` marks the view when it wrote a table in the list. Missing (or no `deps`): every
  SQL commit of the plugin marks the view. An empty list: no SQL commit marks it. The rules of
  `prefixes` apply otherwise (docs/hub.md, "Render dependencies"): a view in flight is always marked,
  a registration clears the declarations.
- The hub knows the written tables from the authorizer: each INSERT, UPDATE and DELETE action names
  its table, also in a trigger. A schema change (CREATE, DROP, ALTER) marks every view of the plugin.
- A rolled-back statement or batch marks nothing. A commit with 0 changes and no schema change marks
  nothing.
- `hub.store.*` commits keep marking by `prefixes`. The two lists are independent.

## 7. Notices from SQL commits

A notice tells an agent that it has new mail. Today the plugin sets and clears notices through
`hub.store.batch` (the `notices` table of hub.db). A plugin on SQL has its data in another file,
so a SQL commit and a notice change cannot be one transaction.

Proposed: the hub owns the outbox.

- `hub.sql.batch` takes `notices: [{agent, pending}]` (`pending` true sets, false clears).
- The hub writes them into `lazyrex_notice_outbox (seq, agent, pending)` of the plugin's file, in
  the same transaction as the statements. A rolled-back batch has no notices.
- After the commit, the hub applies the outbox rows in `seq` order to hub.db, as `hub.store.batch`
  notices are applied today, and deletes the applied rows. At hub start and at the first open of
  the file, the hub applies the rows that remain.
- The hub applies the notices of one plugin one at a time, in commit order. So a clear from an older
  commit can never follow a set from a newer commit. This is the stale-clear protection; the
  plugin needs no notice version, no outbox table and no retry timer of its own.
- A notice for an agent that the hub does not know is dropped and logged, as today.

## 8. Backup

The hub has no backup code today (checked in `hub/cmd/hub` at main 6196f8c; the release
procedures back up hub.db with `sqlite3 .backup`). Proposed: an operator command
`lazyrex-hub sql backup -config <cfg> <dir>` copies hub.db and every `<state>/sql/*.db` with the
SQLite online backup API, through the running hub (a hub action for operators, so each copy is a
committed state) or directly when the hub is stopped. A restore is a manual copy with the hub
stopped. Release and rollback procedures add the SQL files to their backup step.

## 9. Tests

- Unit: the authorizer (each allowed and refused action, also in a trigger body), the value
  conversions and the 64-bit edges, the limits and each error code, the batch rollback on a late
  error and on `expect_changes`, the saved key (replay and conflict), the outbox order, the
  render-dependency marks by table.
- Harness: each lab run has a new state directory, so a new database. `calls()` records
  `hub.sql.*` calls with `sql` and `params` (hub.test.calls records every action). New fault point
  `sql`: `error` at statement n of a batch (rolls back), `lost-reply` (the hub commits, then the
  caller gets a transient error), `hold` (before the commit). `hub.test.sql {plugin, sql, params}`:
  a read-only query for test checks.
- Lab: the replace fence (an old instance's waiting call is refused after the replacement), a crash
  between a commit and the outbox apply, and paging under concurrent writes.

## 10. Other changes in the same work

- The process log: `hub.sql.query` joins the routine reads (a fast, successful call gets no line).
  `hub.sql.exec` and `hub.sql.batch` keep their lines.
- docs/hub.md lists the actions; docs/testing.md lists the fault point and `hub.test.sql`.

## 11. Open points

1. The modernc.org/sqlite version (section 5): the coordinator decides.
2. Rex Lua integer decoding above 2^53 (section 4): I check it on a lab server.
3. The numbers: 1 GiB, 5 s and 10 s, 64 statements, 64 waiting calls, 7 days and 10 000 keys.
4. Section 8: an operator backup command, or a backup only in the release procedures.

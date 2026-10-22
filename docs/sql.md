# Plugin SQL databases: reference (hub.sql)

Status: **proposed, draft 2** (2026-10-21). Nothing here is implemented. Plugin authors read
plugin-sdk.md first; this page is the reference and the hub-side contract.

User decisions (2026-10-21): each plugin gets its own SQLite database, run by the hub, through hub
actions; the plugin-facing interface is the Plugin SDK (plugin-sdk.md), extremely simple; the
machinery stays in the hub. That replaces the part of "LazyRex hub services own namespaced storage
and avoid synchronous call cycles" that said raw SQL is not exposed. Its other rules stay.

Labels: "observed" (with the version), "prototype" (a test in the draft 2 prototype, modernc.org/sqlite
v1.61.0), "untested", "proposed".

## 1. Layers

| Layer | Who uses it | What it has |
|---|---|---|
| SDK (`lazyrex`) | every plugin | `db.exec`, `db.query`, `db.migrate`; one transaction per handler; automatic panel refresh; NULL as nil; big integers as strings |
| SDK advanced options | Messaging, long handlers | `{tx = false}`, `app.notice`, explicit keys and fingerprints, paging, explicit `deps` |
| hub actions (`hub.sql.*`) | the SDK only | sections 3 to 9 |

## 2. The database

- One file for each plugin name: `<state>/sql/<name>.db` (with `-wal` and `-shm`). The plugin name
  keys the KV namespace and the `plugins` table today. A replace, a prune, a source change and a
  hub restart keep the file. The hub never deletes it.
- Fixed settings: WAL, `synchronous = FULL`, `foreign_keys = ON`, `trusted_schema = OFF`, defensive
  mode, `SQLITE_LIMIT_ATTACHED = 0`. The plugin cannot change them (section 6).
- Two connections for each plugin: one writer (all writes and all handler transactions) and one
  read-only reader (reads outside a handler transaction). WAL lets the reader see the last
  committed state while the writer has an open transaction.
- Names that start with `lazyrex_` (any case) are the hub's: `lazyrex_ops` (saved replies) and
  `lazyrex_notice_outbox`.

## 3. Handler transactions

The SDK runs each handler in one transaction. The hub owns that transaction.

- `hub.sql.begin {}` returns `{tx}`, a random token. The hub runs `BEGIN DEFERRED` on the writer.
  The SDK calls it at the first SQL call of a handler (read or write), so every read of the handler
  is in the transaction's snapshot.
- `hub.sql.exec` and `hub.sql.query` with `tx` run in that transaction. Without `tx`, an exec runs in
  its own transaction on the writer, and a query runs on the reader.
- `hub.sql.commit {tx, key?, fingerprint?, reply?, notices?}` commits. With `key`, the hub saves
  `reply` in `lazyrex_ops` in the same transaction (section 8). With `notices`, it writes the outbox
  rows in the same transaction (section 7). The hub replies after the commit. A commit error is the
  reply, and nothing was committed.
- `hub.sql.rollback {tx}` rolls back. It is idempotent.

**Consistency promise.** A handler's reads and writes see one snapshot that starts at its first SQL
call, plus its own writes. No other write to the plugin's file happens between them: the hub has one
writer for each plugin, and the first write of a deferred transaction cannot meet another writer
(the upgrade cannot get `busy`).

Fact (rex-protocol.md, observed on Rex 0.0.1792088193-g7a93ec): a Rex Lua script runs its handlers
one at a time, and a blocking call blocks the whole script. So the SDK never has two open handler
transactions. The hub does not depend on that fact: a call of the same instance without the token,
while a transaction is open, waits on the writer (exec) or reads the committed state (query).

**Lifecycle of an open transaction.**

| Event | Outcome | Test |
|---|---|---|
| The handler returns | the SDK commits; the hub replies after the commit | prototype |
| The handler raises an error | the SDK rolls back | prototype |
| No call for 5 s, or open 10 s | the hub rolls back; later calls with the token get `tx_expired` | lab |
| The plugin client disconnects | the hub rolls back | lab |
| A replace or stop of the instance | section 9: rolled back before the retirement returns | lab |
| Hub shutdown | the hub rolls back, then closes the file | lab |
| Hub crash | SQLite discards the uncommitted transaction at the next open (WAL) | lab (kill -9) |
| A timer or event handler during a handler's blocking call | cannot run (fact above); a lab probe checks it | lab probe |

**Timeouts.** A statement has 5 s. A hub deadline before the commit rolls back and replies `timeout`
with `rolled_back: true`. After the commit, the hub's own work cannot fail the call. If the reply is
lost (the caller's Rex timeout), the outcome is unknown to the caller; the SDK's retry with the same
key replays the saved reply (section 8).

**External effects.** A message sent, a pane opened or a call to another plugin during the handler
is not part of the transaction. The SDK guide says so.

**Opt-out.** `app.action(name, schema, fn, {tx = false})`: each SQL call then runs alone (as without
`tx`). For long jobs, for example a bounded import in steps.

## 4. Statements and values

- One statement for each call; text after it is refused. `?` and `:name` parameters only.
- `hub.sql.query` is read-only: any write action in the authorizer refuses it (also
  `PRAGMA user_version = n`). An exec with `RETURNING` returns its rows.
- Reply of exec: `{changes, id}` (`changes` of the statement itself, `id` = last insert rowid), and
  `rows` for `RETURNING`. Reply of query: `{columns, rows, complete}`.
- Values. Parameters: `null`, booleans (1/0), integer numbers up to 2^53 in size, `{"int":
  "<decimal>"}` for any int64, other numbers as REAL, strings as TEXT, `{"blob": "<base64>"}`. A JSON
  number above 2^53 is refused: Rex Lua already rounded it (observed on Rex 0.0.1792158932-g88fd41:
  Lua 5.1, doubles; rex-protocol.md). Results: INTEGER as a number up to 2^53, else `{"int":
  "<decimal>"}`; REAL as a number (infinite: `unsupported_value`); TEXT as a string (not UTF-8:
  `{"blob"}`); BLOB as `{"blob"}`; NULL as `null`. The SDK turns `{"int"}` into a string, `{"blob"}`
  into a Lua string, and `null` into `nil` in a row keyed by column name.
- `expect_changes: n` (advanced, exec only, DML only): a different direct `changes` fails the call
  with `conflict`; in a handler transaction the SDK then rolls back. `expect_rows: n` (advanced,
  query only): a different row count fails with `conflict`. On any other statement both are
  `invalid_input`.
- Paging (advanced): a query reply holds whole rows up to the reply limit, then `complete: false`.
  `app.db.query` then fails with `too_large`; `app.db.each` reads the next pages as
  `SELECT * FROM (<sql>) LIMIT -1 OFFSET ?<n+1>` in one transaction (observed on hub/sql-r2 c840c92, lab:
  60 rows of 20 KB; the first reply held 51). A first row over the
  limit: `too_large {row: 0, bytes}`. A zero-row reply whose column names alone exceed the limit:
  `too_large`.

## 5. Limits

- Request: the whole `client.perform` message is at most 64 KiB (observed on Rex
  0.0.1792158932-g88fd41). SQL text at most 32 KiB, 999 parameters, a value at most 1 MiB
  (`SQLITE_LIMIT_SQL_LENGTH`, `_VARIABLE_NUMBER`, `_LENGTH`).
- Reply: the reply guard limit, 1 047 552 bytes (observed). The hub sizes the whole encoded reply
  before it commits a call that is not in a handler transaction; in a handler transaction, a
  too-large reply of one call fails that call before the commit, and the SDK rolls back.
- Time: 5 s for each statement; handler transactions as in section 3. Queue residence counts:
  a call that waited 5 s in the plugin's queue fails with `busy` before it runs.
- Size: 1 GiB for the main file (`max_page_count`). Stated limits: the WAL grows between
  checkpoints (the hub checkpoints after commits; a long read can delay it), and temporary files of
  one statement are bounded by its 5 s, not by bytes.
- Errors: `invalid_input`, `refused`, `sql_error` (with the SQLite extended code name), `conflict`,
  `operation_conflict`, `too_large`, `timeout`, `busy`, `tx_expired`, `stale_instance`,
  `unsupported_value`.

## 6. The statement policy

The hub installs a SQLite authorizer on each connection (modernc.org/sqlite v1.61.0
`RegisterAuthorizer`; v1.60.1 has none). The hub's own statements run with the policy off; the
flag changes only between calls, on the hub's goroutine, so a plugin statement never runs with it
off.

| Allowed | Refused |
|---|---|
| SELECT, INSERT, UPDATE, DELETE, all built-in functions (JSON1 too) | ATTACH, DETACH, `load_extension`, virtual tables |
| CREATE, DROP, ALTER of tables, indexes, views, triggers; temporary objects | BEGIN, COMMIT, ROLLBACK, SAVEPOINT, RELEASE (the hub owns transactions) |
| PRAGMA read: `user_version`, `foreign_keys`, `table_info`, `table_xinfo`, `index_list`, `index_info`, `index_xinfo`, `foreign_key_list`, `foreign_key_check`, `integrity_check`, `quick_check` (also as table-valued functions) | every other PRAGMA, and a write of any PRAGMA except `user_version` |
| SQLite's own `sqlite_master` and `sqlite_sequence` actions during DDL | a direct write to `sqlite_master` (defensive mode refuses it) |
| | any name that starts with `lazyrex_` (any case): read, write, create, index, trigger on, rename to, view or trigger body that reads it at use |

Facts behind the policy (prototype, modernc.org/sqlite v1.61.0, 2026-10-21):

- DDL reports SQLite's own `sqlite_master` INSERT/UPDATE/READ actions to the authorizer, some before
  the CREATE action. So the policy cannot refuse `sqlite_master` actions; defensive mode refuses the
  direct writes (also after `PRAGMA writable_schema = ON`, which the policy refuses anyway).
- `ALTER TABLE ... RENAME TO` reports the old name only. After each DDL statement the hub checks
  `main.sqlite_schema` and `temp.sqlite_schema` for reserved names that it did not make, and refuses
  the statement (its transaction rolls back).
- A view body is not authorized at CREATE; it is at each use, also inside a trigger. So a view that
  reads a reserved table can exist, but every read through it is refused.
- `ATTACH` works without the policy; the policy and the limit both refuse it.

## 7. Notices

A plugin wakes an agent with a notice. Through the SDK: `app.notice(agent, pending)` in a handler.

- The notices go into `lazyrex_notice_outbox (seq, agent, pending)` of the plugin's file, in the
  handler's transaction. A rollback has no notices.
- Admission at the commit: the rules of `hub.store.batch` notices today (the manifest's notice
  permission, a known agent, one entry for each agent in one commit). An agent that the hub ever
  knew and that is absent now is accepted: the notice waits for it, as `pending` does today. A
  never-known agent is refused, and the commit fails.
- Apply: the hub applies outbox rows to hub.db in `seq` order, one plugin at a time. Each apply is
  one hub.db transaction that updates `notices` and writes a receipt `(plugin, seq)` in
  `notice_receipts`; a row with a receipt is skipped. Then the hub deletes the applied outbox rows.
  A crash between the two steps replays nothing: the receipt makes the apply idempotent.
- Retry: a failed apply retries with a backoff (1 s to 60 s) from a hub timer, without a later call.
  At hub start the hub scans `<state>/sql/*.db` and applies each non-empty outbox, also of plugins
  that are not configured now.
- One notice path for each plugin: after the first SQL commit with notices, the hub refuses
  `hub.store.batch` notices of that plugin (`notices_moved`). So SQL notices and store notices of
  one plugin cannot reorder. The Messaging cutover commit is that first SQL commit.

## 8. Saved replies (retry safety)

- With a `key`, the commit saves `{key, fingerprint, reply}` in `lazyrex_ops`, in the same
  transaction. A later commit, or a call with the same key before `hub.sql.begin`
  (`hub.sql.replay {key, fingerprint}`), gets the saved reply. The same key with another
  fingerprint: `operation_conflict`. The fingerprint is the SDK's: by default a hash of the action
  name and its arguments; a plugin can give its own (Messaging: its semantic fingerprint).
- The SDK uses the caller's key: the `operation_key` of a UI action (hub.view.action) or the `key`
  argument of an agent call. A call without a key is not saved.
- Guarantee: a retry within 7 days and within the plugin's last 10 000 keys replays. After that the
  key is gone, and a retry runs the handler again. A plugin that needs a longer guarantee keeps its
  own table, and opts out with `{store = false}` (Messaging does).
- The saved replies count toward the 1 GiB. The eviction runs inside the commit (oldest first), so a
  full file can still evict.

## 9. The retirement fence

- The linearization point: the hub marks the instance retired, under the plugin's lifecycle lock,
  before anything else of the replace or stop.
- Then the hub takes the plugin's SQL lock: a running statement ends (at most 5 s), an open handler
  transaction rolls back, and each waiting call of the old instance gets `stale_instance`. Only then
  does the retirement return, and only then can the new instance start. A call of the new instance
  waits behind; it cannot overtake.
- Committed outbox rows of the old instance stay and are applied in `seq` order; they belong to the
  plugin, not to the instance.
- Lock order: the lifecycle lock, then the plugin's SQL lock. No SQL lock is taken under h.mu.

## 10. Render dependencies from reads

- The SDK passes `render` (the render ID that view.render gives the plugin) with each SQL call
  during a panel function. The authorizer records each table that such a call reads (READ actions,
  also through views and triggers; the hub prepares each statement for each call, so no cached
  statement skips the authorizer). When the render ends well, the hub installs these tables as the
  view's table dependencies. A failed render installs nothing (as for declared deps).
- A commit marks the views that read a table that the commit changed. The changed tables: the
  write candidates of the authorizer, kept only when `total_changes()` moved during the call (it
  counts trigger writes, INSTEAD OF triggers, RAISE(IGNORE) after a trigger write, and FK
  cascades; prototype). DDL marks every view. A rollback marks nothing: marks wait for the commit.
- A view in flight is always marked (the existing rule), so a commit during a render is never lost.
- An explicit `deps` in the render reply replaces the tracked one.

## 11. Backup

Version 0: a stopped hub only. `lazyrex-hub sql backup -config <cfg> <dir>` refuses while the hub
holds `hub.lock`. It copies hub.db and every `<state>/sql/*.db` (after a checkpoint) as one set,
and writes a manifest with the file hashes. A restore replaces the whole set with the hub stopped;
pending outbox rows then apply at start (section 7, idempotent by receipt). A coordinated backup of
a running hub is later work.

## 12. Tests (draft 2 prototype and the implementation)

- Prototype, done: the policy (allowed DDL and introspection; each refusal, by case, temp, rename,
  trigger and view bodies at use) and the change marks (RAISE(IGNORE), INSTEAD OF, cascades, a
  no-row update, PRAGMA, DDL).
- To add before implementation approval: handler transaction lifecycle (each row of section 3);
  the timer-during-handler lab probe; the fence (section 9) with a waiting call and an open
  transaction; outbox crash between apply and delete; `notices_moved`; key replay and expiry;
  whole-reply sizing; integers at 2^53 and 2^63-1 through Lua; NULL holes; the read tracking trace
  (a render through a view, a failed render, a commit during a render).

## 13. Open points

1. The bump to modernc.org/sqlite v1.61.0 or a later patch: on main no earlier than 2026-10-24
   (coordinator decision).
2. The numbers: 1 GiB, 5 s and 10 s, 7 days and 10 000 keys, 32 KiB, 999, 1 MiB.
3. SDK delivery (how a plugin requires `lazyrex` from a cache directory): plugin-sdk.md work.

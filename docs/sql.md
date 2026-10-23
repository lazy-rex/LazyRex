# Plugin SQL databases: reference (hub.sql)

Status: **built on branches hub/sql-r2, hub/sql-next and hub/sql-lab, lab only** (2026-10-22). Not on main, not live.
Plugin authors read plugin-sdk.md (the SDK, `require('lazyrex')`); this page is the hub contract
under the SDK. Labels: "built" (code and a test on this branch), "observed" (with the version),
"not yet".

User decisions (2026-10-21): each plugin gets its own SQLite database, run by the hub, through hub
actions; the plugin-facing interface is the Plugin SDK, extremely simple; the machinery stays in the
hub.

## 1. The database (built)

- One file for each plugin name: `<state>/sql/<name>.db` (with `-wal` and `-shm`). A replace, a
  stop, a source change and a hub restart keep the file. The hub never deletes it.
- Settings: WAL, `synchronous = FULL`, `foreign_keys = ON`, `trusted_schema = OFF`, defensive mode,
  no ATTACH (`SQLITE_LIMIT_ATTACHED = 0`), `max_page_count` for 1 GiB. A plugin cannot change them.
- Two connections: a writer (all writes and handler transactions) and a read-only reader (a query
  without `tx` reads the committed state).
- The names that start with `lazyrex_` (any case) are the hub's: `lazyrex_ops` (saved replies) and
  `lazyrex_notice_outbox`.
- Only the plugin's registered instance can call `hub.sql.*`. A plugin registers first, then
  migrates (the SDK does).
- Schema changes follow expand/contract. A rollback runs the previous plugin release on the newer
  schema, so a migration must keep that release working. Expand only: a new table, a new column
  that is nullable or has a DEFAULT, a new index. A destructive change (DROP, RENAME, a new NOT
  NULL column without a DEFAULT, a changed meaning of a column) comes in a later release: only when no
  rollback can go back to a release that needs the old schema. The hub does not check this rule:
  each plugin author keeps it.
- When the database has a newer schema than the plugin knows (`PRAGMA user_version` is greater
  than the number of `app.db.migrate` entries), the SDK runs no migration, does not lower
  `user_version`, logs one line and runs on.
- `hub.db` has no plugin records since the hub.store removal. At each open, the hub drops the tables
  `sql_returns` and `sql_notice_plugins`. It drops `records` and `ops` (the old store) only when each
  namespace with a row in either table has its `<state>/sql/<name>.db` file. Else it keeps both
  tables, logs `hub.store tables kept: namespace "<name>" has rows and no <path>` for each such
  namespace, and tries again at the next open. The file check is a backstop: it does not prove an
  import (`storeretire_test.go`).

## 2. Actions (built)

| Action | Args | Reply |
|---|---|---|
| `hub.sql.begin` | `{}` | `{tx}` |
| `hub.sql.exec` | `{sql, params?, tx?, expect_changes?}` | `{changes, id}`; with RETURNING also `{columns, rows}` |
| `hub.sql.query` | `{sql, params?, tx?, expect_rows?}` | `{columns, rows, complete}` |
| `hub.sql.batch` | `{statements = [{sql, params?, expect_changes?}], tx?}` | `{results = [<exec reply>]}` |
| `hub.sql.commit` | `{tx, key?, fingerprint?, reply? or reply_parts?, notices?}` | `{}`, or `{replayed = true, reply}` |
| `hub.sql.rollback` | `{tx}` | `{}` (also for an unknown or ended token) |
| `hub.sql.replay` | `{key, fingerprint}` | `{found = false}` or `{found = true, reply}` |

- Without `tx`: an exec or a batch runs in a transaction of its own (a batch is all or nothing); a
  query reads the committed state.
- `rows` are arrays in column order. `id` is `last_insert_rowid()`, in the integer form of a cell: beyond 2^53 it is `{"int": "<decimal>"}`. `changes` counts the statement's
  own rows; it is 0 when the statement changed nothing.

## 3. Values (built; observed through Rex Lua)

Rex Lua drops a JSON null in an array and shifts the later values, and it holds integers exactly
only up to 2^53 (observed on Rex 0.0.1792158932-g88fd41: Lua 5.1, doubles). So:

| | Parameters | Results |
|---|---|---|
| NULL | `{"null": true}` (a JSON null is also accepted) | `{"null": true}` |
| INTEGER | a number up to 2^53, or `{"int": "<decimal>"}` | a number up to 2^53, else `{"int": "<decimal>"}` |
| REAL | a number that is not an integer | a number (infinite: `unsupported_value`) |
| TEXT | a string | a string (not UTF-8: `{"blob"}`) |
| BLOB | `{"blob": "<base64>"}` | `{"blob": "<base64>"}` |
| boolean | `true`/`false` as 1/0 | (no boolean type) |

- `params` is an array for `?` parameters, or an object for `:name` parameters (the name without
  `:`). An empty object or array is no parameters (Lua encodes an empty table as `{}`).
- A JSON integer above 2^53 is refused (`invalid_input`): Lua already rounded it.
- A Lua number with no fraction binds as INTEGER (Lua 5.1 has one number type); a REAL column's
  affinity stores it as REAL.

## 4. Handler transactions (built)

- `hub.sql.begin` runs `BEGIN DEFERRED` on the writer and returns a random token. One open
  transaction for each plugin; a second begin waits, then fails with `busy`.
- A call without the token while a transaction is open: an exec or batch waits for it (at most 5 s,
  then `busy`); a query reads the committed state. It never runs inside the transaction.
- A statement in a transaction runs under a savepoint: a failed statement leaves no effect, and the
  transaction continues. The SDK rolls back when the handler fails. Exception: an error after which
  SQLite itself ended the transaction (a conflict with `OR ROLLBACK`, `RAISE(ROLLBACK)` in a trigger,
  some engine errors). Then the whole transaction is undone, the error text ends with "SQLite
  rolled back the transaction", and the token gives `tx_expired`. The hub checks this after each
  failed statement (a `BEGIN` succeeds only outside a transaction).
- The transaction ends at commit or rollback, after **5 s without any hub call of the instance**
  (a non-SQL call such as `hub.agents` counts as activity), after **10 s in total**, when the
  instance retires, and at hub shutdown. After that, the token gives `tx_expired` (or
  `stale_instance` after a retirement).
- The fence: `retire()` rolls back the old instance's transaction before it returns. A call that
  finds a transaction of an instance that is no longer registered rolls it back first. Lock order:
  a plugin's lifecycle lock, then its SQL lock, then `h.mu`.

## 5. Statements and the policy (built)

- One statement for each call. modernc.org/sqlite runs every statement of a text, also in a query
  (observed with v1.61.0: `SELECT 1; DELETE FROM t` deleted the rows), so the hub refuses text after
  the first statement (`sqlite3_complete` finds its end; comments and blanks may follow).
- `hub.sql.query` is read-only: any write action refuses it, also `PRAGMA user_version = n`.
- The authorizer refuses: ATTACH and DETACH; BEGIN, COMMIT, ROLLBACK, SAVEPOINT, RELEASE; virtual
  tables; `load_extension`; every PRAGMA except a read of `user_version`, `foreign_keys`,
  `table_info`, `table_xinfo`, `index_list`, `index_info`, `index_xinfo`, `foreign_key_list`,
  `foreign_key_check`, `integrity_check` and `quick_check` (a write of `user_version` is allowed);
  any use of a `lazyrex_` name (a view or trigger body that reads one is refused at each use).
- After each schema change the hub checks the schema: a `lazyrex_` object that it did not make is
  refused (a rename to such a name reports only the old name), and so is a column declared `DATE`,
  `DATETIME` or `TIMESTAMP`. The driver turns the TEXT of such a column into a Go time and loses the
  text (modernc.org/sqlite v1.61.0, rows.go). Declare a time as TEXT or INTEGER.

Facts from the policy prototype (modernc.org/sqlite v1.61.0): DDL reports SQLite's own
`sqlite_master` actions to the authorizer, some before the CREATE action, so the policy cannot
refuse them; defensive mode refuses a direct write to `sqlite_master`. A view body is not
authorized at CREATE.

## 6. Limits (built)

- Request: the whole `client.perform` message is at most 64 KiB (observed on Rex
  0.0.1792158932-g88fd41). Rex refuses a larger one before the hub sees it, so the SDK measures the
  request first and fails it with `too_large` (then nothing was written).
- SQL text 32 KiB, 999 parameters, a value 1 MiB, the file 1 GiB (`too_large`).
- Reply: the hub stops a query at about 1 040 000 bytes of column names and rows (`complete =
  false`; the SDK pages with keyset paging). A first row over the limit: `too_large`. An exec or
  batch reply (RETURNING rows, column names and the exec fields, as encoded) over the limit:
  `too_large`, before the commit; the statement or batch has no effect. The reply guard limit is
  1 047 552 bytes (observed).
- 5 s for each statement (`timeout`).
- Errors are Rex action errors with a code: `invalid_input`, `refused`, `sql_error` (the text has
  the SQLite code name), `conflict` (`expect_changes`, `expect_rows`), `operation_conflict`,
  `too_large`, `timeout`, `busy`, `tx_expired`, `stale_instance`, `unsupported_value`,
  `no_notice`, `unavailable`. Only `timeout` and `unavailable` leave the outcome
  unknown; every other error means that nothing was committed.

## 7. Notices (built)

- `hub.sql.commit {notices = [...]}` takes three forms (docs/hub.md, "Notices"): `{agent, pending}`
  (the SDK's `app.notice`), `{agent, pending = true, once = true}`
  (`app.notice(agent, true, {once = true})`) and `{agent, read = true}` (`app.notice_read`). The
  admission needs the manifest's `notice`, a known agent, one entry for each agent and no other
  field.
- The notices go into `lazyrex_notice_outbox` in the handler's transaction: a rollback has none.
- After the commit the hub applies the rows to hub.db in `seq` order, in one hub.db transaction that
  writes a receipt `(plugin, seq)` for each row; then it deletes the rows, then the receipts. A row
  with a receipt is skipped, so a crash between the steps applies nothing twice. `seq` is
  AUTOINCREMENT, so a seq is not used again in one file.
- A failed apply does not fail the call: the commit stands, and a timer retries (1 s to 60 s). The
  hub start applies every outbox in `<state>/sql`, also of plugins that are not configured now.

## 8. Saved replies (built)

- `hub.sql.commit {key, fingerprint, reply}` saves the reply in `lazyrex_ops`, in the same
  transaction. A later commit with the key gets `{replayed = true, reply}`, and its own work is
  rolled back. `hub.sql.replay` reads a saved reply without a transaction. The same key with
  another fingerprint: `operation_conflict`.
- A reply over 64 KiB comes as reply parts: `reply_parts = {id, count, bytes}` in place of
  `reply` names a part set that the plugin sent first (docs/hub.md "Reply parts"). The hub joins
  it, saves the joined value, and keeps the set for the reply stub with the same id.
- The hub keeps a reply 7 days and the plugin's last 10 000 keys; the eviction runs in the commit.
- The SDK decides what to save: never a `running` reply. A plugin that needs a permanent replay keeps
  its own table with `store = false` (coordinator decision).

## 9. Render refresh

- Built: a commit (or an exec or batch without `tx`) that changed a table renders the plugin's open
  views again.
- Built: render IDs (docs/hub.md, "Render IDs"). `hub.sql.query` takes an optional `render`, the
  render ID of the `view.render` call that reads. A missing or null `render` is no render; any other
  value that is not a non-empty string is `invalid_input`. A render ID is a read-only token:
  `hub.sql.begin`, `exec`, `batch`, `commit`, `rollback` and `replay` with `render` (any value but
  null), and `hub.sql.query` with `tx` and `render`, fail with `refused` ("a render cannot write").
  The refusal comes before the hub opens the database, so it changes nothing, and an open
  transaction stays usable.
- Built: read tracking, the default for each render (docs/hub.md, "SQL read tracking"). A query
  with an open render ID adds the tables that it read to the render: the authorizer READ actions,
  also through SQL views, joins, subqueries, CTEs and `count(*)` (observed with modernc.org/sqlite
  v1.61.0, renderreads_test.go). A `conflict` (`expect_rows`) adds its reads: the statement ran
  fully. Any other error, also one before the statement runs, makes the render untracked. A
  tracked view (no `refresh` field in its reply) renders again only after a commit that changed a
  table that it read (the write candidates when `total_changes()` moved), or after DDL.
- A render that read no table gets no table set: every commit renders it, also with read tracking.
  A plugin audit looks for the other case: a render that reads tables with tokened queries and
  also shows state outside SQL. Such a view calls `hub.ui.invalidate` (`app.refresh()`) when that
  state changes, or replies `refresh = "all"`.
- The opt-out is `refresh = "all"`. The SDK sends it for a render in which a hub call failed.
- A covered hub read (`hub.agents`, `hub.agents.resolve`, `hub.workers`,
  `hub.profiles`) with the open render ID is a tracked read of its change names (docs/hub.md, "Hub
  reads in a render"). A render that read only change names gets an empty table set.
- Any other `refresh` value is a declaration error (docs/hub.md, "SQL read tracking"): the view gets
  no table set, and `view_deps_invalid` names the value.

## 10. Tests

Unit (`go test ./cmd/hub -run TestSQL`, also `-race`): values and tags, refusals, one statement,
time types, isolation and commit, waiting calls, idle and total limits, busy, stale instance and the
fence, saved replies, batches, paging, notices (outbox, retry, receipts).

Lab (`HARNESS_LAB=1 go test ./cmd/hub -run TestSQLWireLab`): a Rex Lua plugin; NULL in the middle and
at the end of a row, a large integer, a handler transaction, `hub.test.sql`, the commit faults.

Harness (docs/testing.md): `sql()`, `sql_write()`, `inject{match = {sql_commit_writes = t}}`.

Not yet: backup (a stopped hub only, later work), read tracking, a lab test of a hub crash (kill -9)
with an open transaction and with an outbox between apply and delete.

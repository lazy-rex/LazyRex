# Rex protocol notes

> Note: this document was written during the spike phase. Paths such as `spikes/03-ui-framework/`
> refer to the original research repository. In this repository, that code is at the root
> (`hub/`, `view/`, `plugins/`, `tests/`), and each plugin has its own repository.


These notes target **Rex 0.0.1792088193-g7a93ec** (macOS, Go 1.27, module `go.superlogical.com/rex`, links libghostty). Rex is a closed beta. Most items were observed on this build. Items that come from the API reference, inference or proposal are labeled. Re-check each item when Rex changes.

The server documents its own control API:

- `GET /llms.txt` and `GET /api/help` on the server.
- `rex api list`, `rex api describe <method>`, `rex api call <method> '<json>'`.
- Saved copies: [`reference/rex-0.0.1792088193-g7a93ec/`](reference/rex-0.0.1792088193-g7a93ec/).

The saved reference documents the control WebSocket. The terminal data framing (HELLO, WELCOME, GHOSTSNP), the block plugin protocol and the in-band `rex1` messages were not found in it. Those parts come from lab experiments.

## Terms

| Rex term | Meaning | CLI word |
|---|---|---|
| host | One Rex server | host |
| session | A named list of windows. The macOS app sidebar shows it as a header | session |
| window | A tab inside a session | window / tab |
| block | One pane. A terminal is a block with flavor `com.superlogical.terminal.shell` | block / pane |
| principal | The authenticated identity of a connection | — |

Hierarchy: host → session → window → split or floating layer → block. Server limits (defaults, from `rex server run --help`): `--max-sessions` 64 per server, `--max-windows` 64 per session, `--max-panes` 64 per window, `--max-blocks` 1024 per server across all sessions. Detached blocks count toward `--max-blocks`.

## Transport and admission

- The default local server listens on `unix://~/Library/Application Support/rex/server.sock`. `REX_SERVER` in every Rex pane gives the address.
- On Linux, the default endpoint is `unix://$XDG_RUNTIME_DIR/rex/server.sock`. Without `XDG_RUNTIME_DIR`, it is `unix:///run/user/<uid>/rex/server.sock`. A Linux Rex pane has `REX_SESSION` and `REX_BLOCK` but no `REX_SERVER`, so a bare `rex` in it uses this default. With `--autostart=false` and no socket at the default path, `rex` fails (`connect: no such file or directory`) and starts no server. (Observed on Rex 0.0.1792593189-g3f4a0a, Linux, 2026-10-22.)
- A2 check (Linux, the same Rex, LazyRex 63592da, 2026-10-22): one lab server (lab/server.sh) whose socket was the default endpoint of a private `XDG_RUNTIME_DIR`. The hub ran with `-real-server`. The hook ran in an agent pane without `REX_SERVER` and reached the hub: `hub.workers` identity `none`, then `current`. The renderer ran without `REX_SERVER`, with the `-server` endpoint from the hub, and attached (`hub.view.attach`, no error).
- Rex admits a connection or rejects it. It has no per-method authorization.
- Admission kinds: Unix peer credentials (`kind: peer`), tailnet, and `--allow-anonymous` (needed for plain TCP; use it only in a lab).
- `whoami` returns `{kind: "peer", transport: "unix", uid, gid, pid}` for a Unix connection.

## Control channel

- WebSocket `GET /api/control`, subprotocol `rex.control.v1`.
- The client sends `{"type":"hello"}`. The server answers `welcome`. Observed raw frame (lab): `{"type":"welcome","protocol":"rex.control.v1","limits":{"max_message_bytes":1048576},"client":{"id":"client:…","reconnect_token":"…"}}`. The reference example shows only `type`, `protocol` and `limits` (with 65536), so the token location and the limit come from observation.
- Envelopes are JSON: `{"type":"request","id","method","payload"}`, `{"type":"response","id","payload"|"error"}`, `{"type":"event",...}`.
- Methods that take a `session_id` need `session.attach {session_id, event_scope: "session"}` on the same connection first.
- **Events are not replayed.** A client that reconnects must re-read state (`session.view`, `session.list_blocks`).

Useful methods (see `llms.txt` for all):

| Method | Use |
|---|---|
| `session.create {label, initial_windows:[{window_label, layout}]}` | Create a session with windows. A layout is `{block:{flavor, options}}` or `{split:{direction, ratio, before, after}}` |
| `session.new_block` | Create a running **detached** block (no layout space) |
| `session.move_block {block_id, anchor_block_id, direction?, side?, ratio?}` | Place a detached block next to a placed one |
| `session.detach_block {block_id}` | Remove a block from the layout; the process keeps running |
| `session.view`, `session.list_blocks` | Read layout and blocks |
| `session.move` | Reorder sessions in the sidebar |
| `data.connect {session_id, block_id, data_ref_id:"terminal", subprotocol:"rex.data.terminal.v1"}` | Get a data ticket |
| `com.superlogical.terminal.write` | Send input to a terminal |
| `session.own_block` (`take`) + `resize` | Own a terminal's size and resize it |

### Terminal spawn options

`shell`, `command`, `args`, `login`, `cwd`, `path_prepend`, `size`, `theme`, `initial_input`, `exit {on_completion, quick_exit_threshold_ms}`.

**There is no `env` option.** To pass identity or config to a process, wrap the command (`["env", "K=V", "cmd"]`), or rely on the `REX_SESSION` / `REX_BLOCK` / `REX_SERVER` variables that Rex sets in every pane.

### Agent status

Rex detects some programs by itself, with built-in detectors (for example `statusdetect/claude`, and a Codex manifest). A program can also report status itself with OSC 7501. Detector records and OSC reports produce the same event shape, so a record does not show its source. LazyRex uses status as one input to decide when to write a wake notice (see `agent-wake.md`).

Status events (observed by Codex in a lab, same build; full report in `agent-wake.md`):

- `block_event` with `name: "program_status_changed"`, `payload: {id, record: {state, kind?, progress?, app, effective_app, title?, msg?, updated_at, owner: {pid, name}}, reason}`. An empty `id` is the root record. Optional fields are absent when the report does not set them. `msg` is the decoded text, not base64 (lab, Rex 1203: a report with `kind=question` and a base64 `msg` gave `kind: "question"` and `msg: "Allow edit of a.txt?"`). The event has no sequence number and no prompt id.
- `program_status_removed` with `payload: {ids, reason: "clear"}`. Consumers need both events.
- Claude Code (2.1.301) produced `idle`, `working` and `done` with `app: "claude-code"`.
- Codex (CLI 0.162.1) produced `working`, then a **removal** instead of `done`. The built-in detector's `turn_end` rule expects an AM/PM clock (`• 9:13 PM`). This Codex prints 24-hour time (`• 21:13`). The cause is from source inspection; no patched detector was tested.
- A program can report status itself with OSC 7501: `ESC ] 7501 ; state=idle:id=<id>:app=<app> BEL` (or `ESC \`). Colon-separated `key=value`, not JSON. Text fields such as `msg` are base64. Each report replaces the record. `state=clear` removes it. A hook-style shell command that wrote it to `/dev/tty` passed. Installing it as a real agent lifecycle hook is untested. The named hook record in that run had no `owner` field.
- An empty status list does not mean idle. A clear can occur between two queued Codex turns.

Blocked records of real agents (observed by the Telegram reviewer in a lab, Rex 0.0.1792158932-g88fd41):

- Claude Code 2.1.304, a Bash approval: `state: "blocked"`, `kind: "permission"`, `app: "claude-code"`, and `msg` with the request text.
- Claude Code 2.1.304, AskUserQuestion: `state: "blocked"`, `kind: "question"`, and `msg` with the question and its options.
- Codex 0.163.0, a shell approval: `state: "blocked"`, `app: "codex"`, and no `kind` and no `msg`.
- No record had an `id`. Each was the root record.
- **`kind` tells why the program waits, not how it reads input.** In the AskUserQuestion menu, the write `"Blue\r"` (5 bytes written) chose the highlighted option Red, not Blue. A consumer that answers a prompt must choose between raw keys and text plus Enter itself. It cannot infer the form from `kind`.

### Program status needs an attached session

Observed on Rex 0.0.1792158932-g88fd41 (lab `/tmp/rexlab-hub-claude`, 2026-10-20, Go lab test
`TestStatusReadNeedsAttach`):

- `com.superlogical.terminal.program_status` on a pane in a session that the connection has not
  attached fails: `conflict: stream is not attached to session <id>`. After `session.attach` the
  same call returns the records, for example `[{id: "", state: "idle"}]` (the root record).
- After the attach (`event_scope: session`), the hub got the `program_status_changed` events of
  that session: in `tests/harness/tests/notice_ready.lua`, a status change after the first read
  reached the hub. A status that a pane reported before the attach gave the hub no event later
  (the restart test). Whether an unattached connection gets any event of that session: not tested.
- The hub attaches the session before each status read (`readStatus`). Before that fix, an
  identity in another session read its status at hub start without an attach, and the hub kept
  the unknown state until the next status change (defect, live hub, 2026-10-20).
- `session.destroy` also needs the attach: it failed with the same `conflict` error on a session that
  the connection had created but not attached (observed on Rex 0.0.1792158932-g88fd41, Go lab test
  `TestPluginStartUnresolvedIntentLab`, 2026-10-20).

### The pane's process: executable and exec path

Observed on Rex 0.0.1792158932-g88fd41 (macOS lab `/tmp/rexlab-hubrun-claude` and the Linux lab
session, 2026-10-22). `com.superlogical.terminal.process` reports `child` (the pane's process) and
`foreground` (the process in the terminal's foreground) with `pid`, `name`, `path`, `invoked_path`
and `argv0`:

- `path` is the executable, symlinks resolved (on macOS `/tmp/x` is reported as `/private/tmp/x`).
- `invoked_path` is the path given to exec, not argv0: `exec -a /fake/bin/claude /x/tool` reports
  `argv0` `/fake/bin/claude` and `invoked_path` `/x/tool` (macOS and Linux).
- `invoked_path` is reported only while it still resolves to `path`. After the symlink is moved to
  another target (a CLI update), it is null; when the symlink points back, it returns (macOS and
  Linux).
- After the executable's directory is moved, `path` is the new location (macOS and Linux).
- After the executable's directory is deleted, `path` is null on macOS; on Linux it is the old path.
- The CLIs: Codex 0.163.0 reports `invoked_path` `/opt/homebrew/bin/codex` and `path` its Caskroom
  version; Claude 2.1.307 reports `.../bin/claude` and `path` `.../versions/2.1.307`. Neither execs
  itself again. macOS `/bin/sh` does: its `invoked_path` is `/bin/bash`, its `argv0` `/bin/sh`.

The hub records `path` and `invoked_path` at an identity report, and agent restart compares them
(docs/hub.md, "Agent restart").

### Block events (verified)

`rex events -s <session> --json` (or a control connection attached with `event_scope: "session"`) delivers terminal events wrapped like this:

```json
{"type":"session_event","payload":{"session_id":"session:…","event":{"type":"block_event","payload":{
  "block_id":"block:…","creator_name":"com.superlogical.terminal","name":"child_exited",
  "payload":{"exit_code":7,"error":"exit status 7"}}}}}
```

Observed `name` values: `child_exited` (exit code), `process_changed` (foreground process: `pid`, `name`, `path`, `argv0`, `cwd`, `user`, with a `generation` counter). With `exit: {on_completion: false}` the pane stays open after the process exits, so its log stays visible. LazyRex can supervise plugins with `child_exited`.

## Data channel (terminal bytes)

1. Call `data.connect` on the control channel. The `ticket` comes back as a **base64url string**, although the schema says it is a byte array. It decodes to 16 bytes.
2. Open WebSocket `GET /api/data`, subprotocol `rex.data.terminal.v1`.
3. Send binary HELLO: `0x01` followed by the 16 ticket bytes (17 bytes).
4. The server answers WELCOME (binary, first byte `0x81`) or REJECT (`0x82` + reason; `0x05` malformed, `0x01` bad ticket).
5. The next frame starts with a **GHOSTSNP snapshot** (`GHOSTSNP` + u16 version 1). See below.
6. Then raw PTY output bytes.

Rules:

- A ticket binds to the principal that requested it, not to the process or the connection. Observed on Rex 0.0.1792088193-g7a93ec, lab:
  - A ticket requested by one Unix-peer process (which then exited) worked on a data connection from a second process with the same uid: WELCOME `0x81`.
  - The same kind of ticket, used from an anonymous TCP principal, was rejected: REJECT `0x82`, reason `0x01`. So a process that forwards a browser's WebSockets to the Unix socket can carry the browser's own tickets.
- A browser on a different principal cannot use a ticket that LazyRex requested. So a protocol-aware relay is needed **only if the browser neither connects to Rex itself nor goes through a gateway on the same principal**. See "Browser access" below and `architecture.md`, "Web client".
- The data channel only receives. Input goes through `com.superlogical.terminal.write` or `client.key` on the control channel.
- `rex block stream` appears in the CLI help, but this build reports "not implemented".

### The first frame: snapshot plus in-band messages

The frame after WELCOME holds a complete GHOSTSNP blob and then, in the same frame, in-band Rex messages as APC escapes:

```text
ESC _ rex1;<id>;{"type":"resize","cols":80,"rows":24,"cell_width_px":8,"cell_height_px":16,"owner":"client:<uuid>"} ESC \
```

Ghostty's `decodeExact` rejects bytes after the FINISH record. Cut the frame at the end of FINISH before import. GHOSTSNP layout: a 10-byte envelope, then records of `u16 tag, u32 payload_len, u32 crc32c, payload` (little-endian). FINISH is tag 6. Spike 0 (`spikes/00-ghostsnp-restty/`) has the code and the restty result.

Untested: whether `rex1` APC messages also appear later in the stream, and which other message types exist.

## Browser access (verified in a lab)

Observed on Rex 0.0.1792088193-g7a93ec, on a lab server with `--extra-listen http://127.0.0.1:47811 --allow-anonymous --websocket-origin-pattern http://127.0.0.1:47812`. A Node client sent an `Origin` header to stand in for a browser.

| Origin header | Result |
|---|---|
| Matches the pattern | Upgrade accepted. Control `welcome`, `whoami` kind `anonymous`. `data.connect` returns a ticket for this principal. Data HELLO gets WELCOME `0x81`, then a GHOSTSNP frame, then live bytes |
| Does not match | `HTTP/1.1 403 Forbidden` on the upgrade |
| Absent (non-browser client) | Admitted |

A real browser gives the same result (Codex, Chrome, same build). The lab server had `--extra-listen http://127.0.0.1:18793 --allow-anonymous --websocket-origin-pattern 127.0.0.1:18794`. A static page on `http://127.0.0.1:18794` used the browser WebSocket API with no relay. It did these steps:

1. Sent control `hello` and received `welcome`.
2. Created a session and a block.
3. Called `session.attach` and `data.connect`, then decoded the base64url ticket.
4. Sent binary HELLO on its own data WebSocket.
5. Received WELCOME (`0x81`, 32 bytes), then GHOSTSNP (1316 bytes).

The same page on `http://localhost:18794` was rejected. The origin pattern matches the host literally, so `localhost` does not match `127.0.0.1`. The pattern works with a scheme (`http://127.0.0.1:47812`) and without one (`127.0.0.1:18794`).

So a browser page on an allowed origin can be a full Rex client with no relay. On a real host the browser needs a reachable, authenticated transport, such as the tailnet (`--tailscale`), or a paired gateway (the proposed decision "a paired gateway for remote browsers"). Anonymous TCP is for a lab only. Not tested: tailnet admission of a browser, TLS, rendering, input, resize, reconnect.

The macOS app names `https://rex.superlogical.com` as "the hosted Rex client", which is "always allowed" to open WebSockets. `session.open_on_web` is listed beside it, so the action apparently targets that client (inference from app strings). The site redirects to Vercel SSO (private beta), so its exact behavior and transport are not observed.

## Pane environment: `REX_SERVER` is inherited

Observed on Rex 0.0.1792088193-g7a93ec (lab, 2026-10-15):

- Rex sets `REX_SESSION` and `REX_BLOCK` in each pane. It does **not** set `REX_SERVER` for a server started with `--listen`. Panes inherit `REX_SERVER` from the server process.
- A lab server started from inside a Rex pane therefore gives its panes the **real** server's address. A bare `rex` command in a lab pane then reaches the real server. If `REX_SERVER` is unset, `rex` falls back to the default (real) server.
- This happened once in a real-agent test: a lab Claude ran `rex -C lazyrex-plugin:messaging do messaging.send ...`. The call reached the user's real server, failed with "client target not found", and changed nothing.
- Codex's audit of its own lab runs: its explicit test commands used lab endpoints. Its earlier wake runs kept the user's Codex `SessionStart` hook, which runs a bare `rex __internal session-start --agent codex`. With the inherited `REX_SERVER`, that call may have reached the real server. Those runs are not proven isolated. Later runs disabled the hook.
- Lab servers also write outside their `--state-dir`: each one created `~/Library/Application Support/rex/servers/<hash>/start.lock` in the user's real Rex directory (about 20 folders on 2026-10-15). They are startup locks, not state. Not cleaned up yet.
- A pane command is a child of the lab server, in its own process group (observed on Rex 0.0.1792158932-g88fd41, lab `/tmp/rexlab-h1`, 2026-10-19: `sleep` pane pid 92747, pgid 92747, ppid = server 92358). SIGTERM to the server ended the pane process. So a harness that kills the server's process group does not reach panes directly; `lazyrex-hub test` records every descendant and checks each one after the server stops (docs/testing.md).
- Fix: `spikes/lab/server.sh` starts the server with `REX_SERVER=unix://<lab>/rex.sock` and without the inherited `REX_SESSION`, `REX_BLOCK` and `REX_CLIENT`. Verified: `rex whoami` in a lab pane then reports the lab endpoint.

## Network admission without a tailnet

From `rex server run --help` and `rex login-server --help` (this build):

- Rex authenticates network callers only through the tailnet (`--tailscale`, Tailscale identity). On plain TCP it can only admit anonymous callers (`--allow-anonymous`), and "every connection gets full access".
- `rex login-server` runs as root. It maps callers (Unix credentials or tailnet identity) to OS accounts and proxies them to per-account Rex servers. It does not add another authenticated network transport: on plain TCP it also admits only anonymous callers.
- So without a tailnet, a remote browser needs a LazyRex component that authenticates it.

## Client RPC: `client.perform`

From the API reference, plus a lab test on Rex 0.0.1792088193-g7a93ec.

- A client declares actions with `client.keymap {actions:[{name, kind:"client", title}]}`.
- Another client calls `client.perform {client_id, action, args, timeout_ms}`. The server sends the target a `client_perform_requested` event and waits for `client.reply {call_id, result | error}` from the target, on the connection that received the event.
- **The server attests the caller.** The event carries `requester: {client_id, info, principal: {kind, uid, gid, pid}}`. The reference says: "a client cannot claim to be another." With the pid, the hub can walk process ancestry to the caller's pane without its own socket.
- Requests to one client queue and are answered one at a time. `timeout_ms` defaults to 10 s, maximum 30 s. At most 16 requests can wait. A disconnect fails waiting requests with `client_unavailable`. A timeout does not cancel the action: "the client may still perform the action later".
- Observed on Rex 0.0.1792158932-g88fd41 (lab, `TestChangedBurstLab` before the coalescing fix): a 17th waiting request is refused at once with code `client_unavailable` and the text `<client> is busy: 16 requests are already waiting`. A refusal is not a timeout: the client never sees the request.
- Observed on Rex 1203 (lab, `spikes/lab/perform-queue.mjs`): Rex delivers the next `client_perform_requested` to a client only after that client replies to the current one. A call that the client holds for 3 s delays the next call by 3 s. So a hub action that waits (a long poll, a slow Rex call) stalls every other caller of the hub. Hub actions must answer fast.
- Observed on Rex 1203 (lab, `spikes/lab/block-event.mjs`): `block.event` with a custom type fails ("event type … is not supported by this server"). A client cannot publish its own event types through Rex. Correction (Rex 1203, lab, a391cee): `block.event` with type `block_event` and any name and payload reaches a plugin block unchanged; a plugin can emit `block_event` frames that subscribers see as session events. See "Plugin event emission".
- The `client.reply` error is `{code, message}` only: the input schema has `additionalProperties:
  false` (`rex api describe client.reply`, Rex 0.0.1792158932-g88fd41, 2026-10-20). So a hub error
  carries its details in the message text, not in extra fields.
- Observed on Rex 0.0.1792158932-g88fd41 (lab, `TestDigestLab` in hub/cmd/hub/digest_lab_test.go, 2026-10-21): a
  Lua string that is not UTF-8 does not reach the receiver unchanged. Rex replaces each byte that
  is not part of a valid UTF-8 sequence with U+FFFD (EF BF BD). A Lua plugin sent `a\255b` to
  another Lua plugin with `client.perform`; the receiver got the bytes `61 ef bf bd 62`. A lone
  `\195` arrived as `ef bf bd`. No error. So a body that an agent or a plugin sends through Rex is
  valid UTF-8 when it arrives, and its original bytes are lost.
- `rex -C <label> do <action> k=v` calls `client.perform` from the CLI. `client.set_label` sets `info.label` in `rex client ls` (the top-level `label` stays null).
- So Rex can carry hub RPC itself: an agent or a plugin calls the hub's actions, and Rex tells the hub who called. **Not yet tested from a control client that can reply** (a Node or Go client). See the next section for the Lua result.

## Rex Lua runtime: `rex do`

Observed on Rex 0.0.1792088193-g7a93ec, lab server. Probe: `spikes/lab/perform-probe.lua`.

- `rex do script.lua` runs gopher-lua (`_VERSION` "Lua 5.1") in the CLI process, as a Rex control client.
- Available: `io` (including `io.open`, `io.popen`), `os` (including `os.execute`, `os.getenv`), `require`, `package`, `load`, `loadstring`, `coroutine`, `debug`, and the gopher-lua `channel` global.
- Missing: sockets, JSON, SQLite, `utf8`. Correction (observed on Rex 0.0.1792158932-g88fd41, lab, 2026-10-19): `rex.json.encode` and `rex.json.decode` exist (Rex docs, "rex.json"). `rex.json.encode({a = "x\0\""})` gave `{"a":"x\u0000\""}`, and an empty table encodes as `{}`. `rex.json` has no other members (only `encode` and `decode`), and `rex.json.decode('[]')` gives a table with no metatable that encodes as `{}` again. So a `rex do` script cannot send an empty JSON array (observed on Rex 0.0.1792158932-g88fd41, lab, 2026-10-19). The test harness sends one through its Go test agent (`call_raw`, docs/testing.md). A pure-Lua JSON library can be loaded with `require`. SQLite would need the `sqlite3` CLI through `io.popen`.
- **Long argument lists and `table.concat` fail with "registry overflow"** (observed on Rex 0.0.1792158932-g88fd41, lab, 2026-10-22, `rex do` scripts). The first report: Messaging, a 49 KB BLOB in a `db.query` row. Each value is the last count that passed at the top level of a script; the next count failed:
  - `table.concat(t)` of one-character items: 2559 (2560 failed). A separator does not change it (8000 items with `,` failed). `table.concat(t, sep, i, j)` of a range under the limit passes.
  - A call `f(unpack(t))`: 2583 arguments.
  - `select('#', unpack(t))`: 5128. `string.char(unpack(t))`: 5139. `select(n, unpack(t))`: 5148. `string.format` with n `%s` arguments: 5157. `{unpack(t)}` of 6000 items failed.
  - Inside `pcall`, each failure is a catchable error "registry overflow". Outside `pcall`, the first probe showed other messages (a number, or the string itself).
  - The limit is lower when more of the stack is in use: in the SDK test runner, `table.concat` of 2000 items passed and of 2559 failed. So the counts above are upper bounds.
  - The SDK sets `table.concat` to a version with no limit (`concat` in `sdk/lua/lazyrex.lua`; `boot.lua` installs it). It joins ranges of at most 500 items, then the blocks. A plugin must still not `unpack` a long list.
- **Numbers are doubles: an integer above 2^53 is not exact** (observed on Rex 0.0.1792158932-g88fd41, lab, 2026-10-21). gopher-lua is Lua 5.1: no `math.type`, no `math.maxinteger`, no integer subtype. `rex.json.decode('{"a":9007199254740993}')` gives 9007199254740992, and `rex.json.encode` writes 9007199254740992. A plugin that needs exact 64-bit values (revisions, cursors) keeps them as decimal strings.
- Network (lab): `require("socket")` and `require("http")` fail. `package.cpath` is empty and `package.loadlib` raises "loadlib is not supported", so C modules cannot load. Outbound HTTP works through a helper process: `io.popen("curl ...")` returned `200`. No native WebSocket.
- `io.read` reads the script's stdin (lab: a JSON line piped into `rex do -e` came back intact). So a `rex do` script could act as a stdio program, for example an MCP stdio server. Untested as MCP.
- Other clients reach a Lua script through Rex: an anonymous TCP client (`rex -S http://127.0.0.1:<port>`) called a `rex do` script's action with `client.perform` and got the result (lab). Its `requester.principal` has no `pid`, so a hub must handle every principal kind.
- Rex API: `rex.call` (any control method), `rex.on` (events), `rex.action {name, title, run}`, `rex.invoke`, `rex.wait`, `rex.sleep`, `rex.stop`, `rex.log`, `rex.server(label)`, `rex.block.call`, `rex.client.queue`, `rex.layout.*`, `rex.args`.
- On Rex 1203 (`0.0.1792158932-g88fd41`), a failed `rex.call('client.perform', ...)` returns `nil` and an error. It does not raise the error. Check both return values before reading `response.result`. Observed in the hub home test with an RPC error from `hub.profiles`.
- A script that registers `rex.on` handlers keeps running and runs handlers one at a time. `rex.sleep` and `rex.wait` block everything else. Without `-s`, `--all` or a session in the environment, it fails with "no sessions".
- `rex.on` handlers receive `(ctx, event)` (observed for `client_perform_requested`).
- **After a caught error, a closure's write to a captured local can be lost.** Minimal case: `local n = 0; local inc = function() n = n + 1 end; pcall(error, 'x'); inc()` leaves `n` at 0 (standard Lua: 1). Probe: `lab/probes/closure-after-pcall.lua`. Observed on Rex `0.0.1792158932-g88fd41`, lab, 2026-10-20, as a `rex do` script (hub team): minimal 0, a catch in a deeper frame 0, a `pcall` that catches nothing 1, a table field 1. Contexts reported by other teams on the same build:
  - A `rex do` script: reproduced (Tunnels, 2026-10-19; the first report).
  - A plugin request handler: reproduced (Worktrees, a temporary probe action).
  - The test runner: intermittent. Lost in a nested function, and in a `wait` callback after other catches had run. Correct in a test body in one file (Worktrees). Not reproduced in a test body (Templates reviewer).
  - The runner's own xpcall handler: reproduced (hub team, 2026-10-19, fixture `tests/harness-fail/tests/location_after_catch.lua`). The test body called `expect_error` and a `wait` whose callback failed once, then failed an assertion. The handler's write to the runner's captured local `loc` was lost, so the failure had no `(at file:line)`. A test that failed with no catch before it kept its location. The runner now returns the location in the handler's result.
  - A plugin main chunk: reproduced (Worktrees, a temporary probe in a plugin). A catch during the main chunk splits module-level locals too. The main chunk defined `local n = 0`, a setter and a getter, then ran `pcall(error, 'x')`, then defined a second getter. Later, in a request handler, the setter and the first getter saw 1, then 2. The second getter and a direct read in the handler saw 0. So module-level locals are safe only when no catch runs in the main chunk before the closures that share them run.
  - Wider trigger and xpcall (hub lead and SDK team, observed on Rex `0.0.1792158932-g88fd41`, lab, 2026-10-22). Probe: `lab/probes/xpcall-after-catch.lua`. A function makes local `n` and closure `f`. `f` adds 1 and raises on its first call. The function runs `pcall(f)`, then `f()`, then returns `n` (standard Lua: 2).
    - The catch can be anywhere while the function still runs: in the closure, in a function that it calls, or in the function itself. The split goes both ways: a write in the closure is lost to the function, and a write in the function is lost to the closure. Bare and under `pcall`: 1.
    - Under an active `xpcall`, with no earlier protected call inside it: 2. After one protected call (`pcall` or `xpcall`, with or without an error) finished inside that `xpcall`: 1. After the `xpcall` returned: 1. In a coroutine resumed inside the `xpcall`: 1. So `xpcall` is not a workaround.
    - `xpcall(f, h, a)` does not pass `a` to `f`: `f` gets `nil`. This is standard Lua 5.1 (extra `xpcall` arguments came in Lua 5.2). Wrap: `xpcall(function() return f(a) end, h)`.
    - `pcall(rex.call, 'no.such.method', {})` does not split: `rex.call` returns `nil` and an error, so nothing is caught.
    - A plugin request handler (SDK team, a plugin process): reproduced in both directions. The SDK caught a refused `hub.ui.invalidate` in `app.refresh` while the handler ran, and the handler's local lost a write. `040ee9b` removed the protected calls from `hub()` and `app.refresh()`. Other SDK paths still use protected calls (around handlers, form rendering, commits and callbacks).
    - The test runner calls each test function under `xpcall`. A test can still split: `wait` and `expect_error` use `pcall`, so the first finished protected call ends the protection.
  - Rule: no closure writes, or reads, a local that changes, where a protected call can run or an error can be caught while the function that owns the local still runs. This includes module-level locals when the main chunk catches an error. Use a table field or a return value. The harness `wait` returns its callback's result (docs/testing.md, "Waiting").
  - Audit (hub team, 2026-10-19, same build; rechecked 2026-10-22 against the wider trigger). No main chunk below catches an error, and each main chunk returns before a handler or a test runs, so the function that owns these locals no longer runs when the catch happens (SDK team, a plugin process: module-level locals written after the main chunk returned stay correct). Each closure write to a module-level local is listed with its evidence:
    - `plugins/lazyrex.lua` (now `plugins/lazyrex/lazyrex.lua`) `navigation_notice`: the launch action writes it after a `pcall` that caught a failed navigation, and render reads it. Observed correct: `tests/home/tests/navigation.lua` (now `plugins/lazyrex/tests/navigation.lua`), "a launch whose navigation fails still reports the launch and shows a notice", after earlier caught action failures in that file.
    - `lab/test/harness.lua` `A`, `self_id`, `hub_id` in `H.setup`: written before any test runs.
    - `lab/test/harness.lua` `hub_id` in `H.restart_hub`: observed correct in a probe. A test body caught two errors (`expect_error`, and a `wait` callback that failed once), after an earlier failed test. Then it restarted the hub. A later `H.hub` call reached the new hub.
    - `lab/test/harness.lua` operation key counter: now a table field. A probe after a failed test showed no lost write (hardening, not a reproduced defect).
- **A generic `for` header that indexes or concatenates a function call result before a method call or a field read evaluates wrong.** In most cases Rex raises no error. Found by the Tunnels implementer, observed on Rex `0.0.1792158932-g88fd41` (lab, in a fresh test, no `pcall` involved). `P` is a line pattern, for example `"[^\n]+"`. In the `.s` examples, `f()` returns the table `{s = "aaa\nbbb\n"}`. In the direct `gmatch` and the concatenation examples, `f()` returns the string `"aaa\nbbb\n"`:
  - `for x in (f().s or ''):gmatch(P)`: no iteration.
  - `for x in f().s:gmatch(P)` and `for x in (f()).s:gmatch(P)`: "attempt to index a non-table object(nil) with key gmatch".
  - `for x in f().s.gmatch(f().s, P)`: the same nil error.
  - `for x in (f() .. ''):gmatch(P)`: a Go "runtime error: invalid memory address or nil pointer dereference".
  - These work: `for x in f():gmatch(P)`, `for x in getobj():items()`, `for x in string.gmatch(f().s, P)`, `for x in (t.s or ''):gmatch(P)` with a local `t`, `local v = f().s:upper()` outside a `for`, and `for i = 1, #f().s`.
  - A call result's field as an argument also works: the hub tests use `for _, w in ipairs(a:hub('hub.workers', {}).workers) do`, and they pass.
  - Side note: `s:gmatch(P)` returns an iterator that needs its hidden state. `local it = s:gmatch(P); for x in it do` fails ("bad argument #1, userdata expected, got nil"). Use `gmatch` only directly in a `for` header.
  - Rule: put the value in a local before the `for`.
  - Audit (hub team and hub reviewer, 2026-10-19, source only): `plugins/lazyrex.lua`, `lab/test/*.lua`, `lab/probes/*.lua` and `tests/**/*.lua` have none of the failing forms. The one `gmatch` loop (`tests/isolation/tests/isolation.lua`) uses a local string.
- **A `rex do` script sends at most 64 KiB in one control message** (observed on Rex 1203, `0.0.1792158932-g88fd41`, lab, 2026-10-18 and 2026-10-19). The CLI reports `control message too large: encoded request is 65541 bytes, limit is 65536`. This matches the Control v1 reference (`max_message_bytes: 65536`, for the whole decompressed envelope). It covers the script's requests (for example `hub.store.batch`) and its `client.reply`. A `client.reply` that is too large never arrives: the caller sees only a timeout (a plugin reply with two 50 000-byte records: no answer within 10 s, 2026-10-19). A request that is too large fails at once (66 228 bytes, 2026-10-19). Effects in LazyRex:
  - A view tree from a Lua plugin must stay well under 64 KiB. Before 2026-10-18 the messaging plugin sent up to 200 messages. A thread whose newest 200 messages were larger than about 64 KiB did not render (the hub kept the old tree). Five live threads were 52 to 473 KB.
  - The messaging plugin now sends the newest messages within a 40 KB estimate, minus the draft. The transcript pane gets older pages through `hub.view.history` (each page within 40 KB) and prints the whole thread.
  - Correction (2026-10-19): an earlier line here said that `hub.store.get` replies are limited too, so the plugin read message records 8 at a time. That was an inference. The next item has the measurement.
  - A send to a group writes every member's mailbox in one batch. In the lab, Team sends failed after 287 messages: four mailboxes of 13 KB and the thread record passed 64 KiB. Live mailboxes stay small while agents read their mail. Not fixed (roadmap, Known defects).
- **A `rex do` script receives more than 64 KiB in one reply** (observed on Rex `0.0.1792158932-g88fd41`, lab, 2026-10-19, the harness fixture plugin; probe `lab/probes/plugin-message-limits.lua`). The plugin called `hub.store.get` through `client.perform` and measured the result with `rex.json.encode` (`lua_bytes`):
  - Two records of each content class arrived in full, with Go-encoded text of 65 592 to 65 600 and 99 996 to 100 000 bytes: ASCII, `<`, U+2028, `é`, U+0001, and `"\`. `lua_bytes` was 65 679 to 65 689 and 100 084 to 100 089.
  - ASCII records of 49 960 bytes: 4, 10 and 20 records arrived (`lua_bytes` 200 005, 499 994 and 999 984). 21 records were over the hub's limit. Before the reply guard, the hub lost its connection ("disconnected while the request waited", `client_unavailable`). With the guard, the plugin got `reply_too_large` (1 050 091 bytes, limit 1 047 552).
  - Inferred, not observed beyond these sizes: a plugin receives up to the sender's limit. The largest observed reply to a plugin is 999 984 bytes.
- **A frame from a client over the welcome's `max_message_bytes` closes that client's connection** (observed on Rex `0.0.1792158932-g88fd41`, lab, 2026-10-19, `TestServerFrameLimitLab` in `hub/cmd/hub/replyguard_lab_test.go`). The Go client on the Unix socket was welcomed with 1 048 576.
  - A `client.reply` frame of 1 048 576 bytes was accepted. One of 1 048 577 bytes closed the replying client (`client.inspect`: not_found). The caller got `client_unavailable`.
  - The server's response to the caller is larger than the reply frame. A Go caller (limit 1 048 576) got the result of a 1 048 544-byte reply frame (result 1 048 456 bytes). For a reply frame of 1 048 560 bytes or more, the caller got `too_large: control response exceeds 1048576 bytes` at once, and the replying client stayed connected.
  - The hub's reply guard (`hub/cmd/hub/replyguard.go`) keeps every reply frame at or below `max_message_bytes` minus 1 KiB (1 047 552 bytes). Over that, the hub sends `reply_too_large` with the attempted size.
- **`client.perform` into a `rex do` script works.** Declare the action with `client.keymap`. In `rex.on("client_perform_requested", function(ctx, ev) ... end)`, call `rex.call("client.reply", {call_id = ev.call_id, result = ...})`. The caller gets `result`. `ev` has `call_id` (a string), `action`, `args` and `requester {client_id, info, principal {kind, uid, gid, pid}}`. A handler return value alone does **not** reply. The script keeps state between calls. The CLI can call it: `rex -C <label> do <action> k=v --json`. Probe: `spikes/lab/perform-probe.lua`.
- A normal control client (Node) also answers with `client.reply` on the connection that received the event (Codex's positive control, same build).
- Correction: an earlier version of this section said the reply timed out. That was a probe bug: the handler read `call_id` from `ctx`, not from the event.
- `rex.action {name, title, run}` in a `rex do` script did not make the action callable from another client: `rex -C <client> do <name>` failed with "no action named". Declaring it with `client.keymap` made the call reach the script (see above).

### Plugin timers (reviewer lab, 2026-10-20)

Observed on Rex `0.0.1792158932-g88fd41`, in an isolated reviewer lab, with
`lab/probes/timer-every.lua` and `lab/probes/timer-only.lua`:

- `rex.every(2)` continued after a handler caught an error with `pcall`.
- At 17 seconds, the probe reported 8 ticks, one caught error and `ctx.origin = "timer"`.
- A seven-second handler using `rex.sleep` blocked timer callbacks. The tick count stayed at 8.
- After the handler returned, one callback ran. The count became 9, with no callback during the hold.
- At 37 seconds, the count reached 16. `rex.after(1)` had run once.
- A script with only `rex.every(1)` and no `rex.on` handler remained active through 36 printed ticks.

This check did not test macOS sleep, intervals of hours, or timer persistence across a process restart.

Implementer lab (`/tmp/rexlab-hubf-claude`, same build, 2026-10-20): the same probe, started in a
detached block as the hub starts plugins, reached 272 ticks at 549 seconds with a 2-second interval.
That is one run of about 9 minutes. It does not test macOS sleep or intervals of hours either.

A plugin that needs a daily job (the Automations health check) uses a short tick and the wall
clock, not a timer of one day. For example, `rex.every(60)` compares the time with a `last_run` record
in its hub store. It also checks at start, so a run missed while the plugin or the hub was down
runs at the next start.

## Durability

- Terminal processes are direct children of `rex server`. When the server stops, they stop. The state dir kept only `host-id`. There is no hot handoff.
- LazyRex accepts this: the Rex server is the stable core, and LazyRex can restart without losing terminals.

### A request outlives its client

Observed on Rex `0.0.1792158932-g88fd41`, lab, 2026-10-21. A client wrote one create request
(`session.new_window`, and in a second run `session.new_block`) and exited at once, without
reading the answer. Rex completed both requests:

- A search of the session 30 ms after the write found no pane with the request's label.
- A search 1 s after the write found the pane.

So a request that a client wrote can still take effect after the client is gone, and one empty
search does not prove that the request had no effect. Only a request that was never written proves
that nothing was created. The hub records a spawn as `sending` before it writes the request, and
treats a `sending` spawn as unknown, never as failed (hub.md, "Lifecycle").

## Caller identity

- The location that `whoami` reports comes from the caller's `REX_SESSION` / `REX_BLOCK` environment. **A caller can spoof it** with another real block ID.
- The peer `pid` comes from socket credentials and is reliable. Walk the parent chain from that pid up to a pane's child pid (`rex block call <block> process`) to find the caller's pane.
- Limits: double-fork, `setsid` and `nohup` can break the chain. TCP has no pid. All processes run as the same uid, so this is attribution, not a security boundary.

## Restricting Rex access to LazyRex only

Rex cannot authorize per method. An OS sandbox can block agents from the Rex socket:

```scheme
(version 1)(allow default)
(deny network-outbound (remote unix-socket (path-literal "<HOME>/Library/Application Support/rex/server.sock")))
```

With `sandbox-exec`, this profile made `rex --autostart=false whoami` fail with "operation not permitted". Other Unix sockets still worked. Still to do: block autostart (deny exec of `rex` or writes to the Rex runtime dir). Linux: Landlock or bubblewrap. A hard boundary needs a separate OS user.

## Block plugins (executable services)

`rex server run --block-dir DIR` starts each executable in DIR at server start. Rex executes the file directly, so a script needs a shebang or a shell wrapper:

1. Rex passes `REX_BLOCKSERVER_INFO_FD=3`. The plugin writes one JSON line to fd 3 and closes it: `{"network":"unix","addr":"/path/p.sock","base_url":"http://plugin","pid":N}`. Optional: `sdk_version`, `features`. The field is `addr`; `address` fails.
2. Rex connects to the plugin's `/api/control` with `rex.control.v1`, roles reversed. The plugin answers `welcome` and must answer WebSocket pings (Rex drops it after 2 missed pongs).
3. Rex calls `block_creator.list`. Each flavor needs a `description`. Method `actions` appear in the command palette as `<creator>.<action>`.
4. `block.create {flavor, options, session_id, block_id}` must return `{block_id:"block:<uuid>"}`, or Rex resends it.
5. `block.method {block_id, method, args}` carries **no caller identity**. See "Block method caller identity".
6. `block.event` delivers lifecycle events and session-wide block events.

Limits: no restart after a crash (calls then fail with `blockexec/blockremote: remote control disconnected`); `--block-plugin-reload` reloads only when the executable's content hash changes; new executables are found only at server start.

Lab plugin: `spikes/lab/fake-block-plugin.mjs`.

Plugin events (observed on Rex 1203, lab `/tmp/rexlab-ev`, `spikes/lab/block-emit-probe.mjs`, 2026-10-16):

- After `block.create`, Rex sends the plugin `block.event` requests for its block: `session_attached`, `owner_changed` (reasons `create`, `disconnect`), `focus`, `session_view_changed`, `client_connected` and `client_disconnected`.
- `--block-plugin-reload`: a change to the plugin executable restarts the plugin. The block stays in the session, and Rex sends `session_attached` again to the new process.
- The first probe found no emission path. Its frames used the wrong envelope (see below). Superseded on 2026-10-16.

### Plugin event emission (observed on Rex 1203, lab `/tmp/rexlab-bp`, 2026-10-16)

Found by disassembly of `sdk/blockremote`, `sdk/blockserver/apiclient` and `sdk/blockserver/apitypes` (event.go), then proved in the lab with `spikes/lab/block-emit-probe.mjs`.

- Rex reads `event` frames that the plugin sends on the control connection it opened to the plugin. The event goes in **`payload`**, not in `event`:

  ```json
  {"type":"event","payload":{"type":"block_event","payload":{
    "block_id":"<the plugin's own block ID>","name":"lazyrex.example","payload":{"any":"json"}}}}
  ```

- `block_id` is the ID that the plugin returned from `block.create`. Rex maps it to its own block ID. An event with Rex's block ID is dropped without a log line.
- Rex publishes it to the session like a terminal event. Observed in `rex events -s <session> --json`:
  `{"type":"session_event","payload":{"session_id":…,"event":{"type":"block_event","payload":{"block_id":"<Rex block ID>","creator_name":"dev.lazyrex.probe","name":"lr_B","payload":{"v":"B"}}}}}`. Rex sets `creator_name` itself. The plugin cannot set it.
- `name` and `payload` are free. No feature flag and no `sdk_version` is necessary.
- Rex decodes three event types from a plugin (`blockserver/apitypes` event.go). Other types become `UnknownEvent` and Rex drops them:

  | `type` | `payload` | Observed effect |
  |---|---|---|
  | `block_event` | `{block_id, name, payload}` | Session `block_event`, as above. |
  | `block_action` | `{block_id, action:{type, payload}}` | See the action types below. |
  | `block_closed` | `{block_id}` | Rex removes the block from the session and publishes `block_closed`. Rex does not send `block.close` back. |

- Action types (`sdk/block/action` json_action.go):
  - `{"type":"event","payload":{"name","payload"}}`: same result as `block_event`. Observed.
  - `{"type":"close","payload":{}}`: Rex closes the block, publishes `block_closed`, then sends `block.close` to the plugin. Observed.
  - `{"type":"api","payload":{"target":"<block ID>","method","id","args","caller"?}}`: Rex accepted the frame and did nothing. A `com.superlogical.terminal.write` to a terminal block in the same session wrote nothing, with a Rex or a plugin block ID as `block_id`. No log line. Rex's `forwardEvents` has a branch that drops some action types; the API action seems to fall in it (inference from disassembly).
- **A malformed frame kills the plugin.** `{"type":"event","event":{…}}` (no `payload`) made Rex close the connection (`control event payload is required`, WebSocket close "malformed control envelope"). Rex does not reconnect. Every later call fails with `blockremote: remote control disconnected` until a reload or server restart.

### Plugin handshake fields (observed on Rex 1203, lab)

- `sdk_version` (string) and `features` (array of strings) in the fd-3 JSON appear only in the startup log line `block plugin ready … sdk_version="" features=[]`. The reload log line does not show them.
- No API shows them: `server.status`, `server.version`, `server.whoami`, `block_creator.list` and `rex api list` did not contain them with `sdk_version:"9.9.9-lazyrex"`, `features:["block_events","lazyrex.test"]`.
- Rex accepts any strings. Disassembly of `blockexec`, `blockremote` and `internal/blockplugin` found no comparison against a feature name. Event emission works with `features: []`. So no recognized value is known, and no behavior change was found.
- A non-string item in `features` (an object) makes the start fail. During `--block-plugin-reload`, Rex then retries about every second, with `reloading block plugin failed` and no reason in the log.
- The `welcome` that the plugin sends is not checked beyond `type`: the probe's welcome has no `client` field.

### Creator-level methods (observed on Rex 1203, lab)

- None. The server has only the block method path (`blockMethodHandler`, `decodeBlockMethodBlockID`). Rex sends a plugin only `block_creator.list`, `block.create`, `block.method`, `block.event` and `block.close`.
- `dev.lazyrex.probe.echo` without `block_id` resolves to the session's focused block. With a terminal focused: `method not found: "echo"`. Without `session_id`: `session_not_found`.
- So a plugin needs a block instance. A detached block costs no layout space: `session.new_block {session_id, flavor, label}`. Rex sends `block.create` without `options` when the caller gives none.
- `--block-plugin-reload` starts the new process, then sends `block.create` again for each existing block. The plugin may return the same block ID.

### What a plugin can declare (Rex 1203)

From struct tags in the binary and the embedded `block_creator.list` example:

- Creator: `name`, `title`, `description`, `flavors`, `api.methods`.
- Flavor: `name`, `flavor` (full name), `title`, `description` (required), `options_schema`.
- Method (`BlockMethodDesc`): `description`, `input_schema`, `output_schema`, `actions`.
- Action (`BlockActionDesc`): `name`, `title`, `description`, `category`, `keywords`, `args`, `repeats`.

Where they show (observed, lab):

- `rex api list`: one row per method, `<creator>.<method>` with its description, under a CREATOR column.
- `rex api describe <creator>.<method>`: description, input and output schema, and a note to put the input in `args` next to `session_id` and `block_id`.
- `client.action {action:"<creator>.<action>", block_id}` runs the action. Rex calls `block.method` with the method and the action's declared `args` (here `{}`). A request with `args` fails: "action … has fixed arguments, so the request must not include args". So `args` in the declaration is a fixed preset. Without `block_id`, Rex uses the focused block and fails with `409 conflict` when it is not a block of that creator.
- The command palette shows actions (see "Block plugins" above). `repeats` and the app's use of `category` are not tested (no app in the lab).

### Client to plugin events (observed on Rex 1203, lab)

- `block.event {session_id, block_id, event}` from any client reaches the plugin as a `block.event` request.
- Rex accepts only event types it knows. `lazyrex.custom` fails: `event type "lazyrex.custom" is not supported by this server`.
- `block_event` is a known type. So a client can send `{"type":"block_event","payload":{"block_id","name":"ext","payload":{…}}}`, and the plugin receives it unchanged. This is a free-form client-to-plugin channel. Rex does not publish it to the session stream.
- `focus` and `key` also arrive. Rex fills missing `key` fields (`mods`, `text`, `action:"press"`, …).

### Default plugin directories (Rex 1203, from disassembly, unverified)

`blockplugin.DefaultDirs` joins `rex/blocks` to two base directories (probably the user config directories). `serve.go` calls it. The exact paths and whether `--block-dir` replaces them are not tested.

## Block method caller identity

Observed on Rex 1203 (`0.0.1792158932-g88fd41`), lab, 2026-10-16, unless a line says otherwise. Probes: `spikes/lab/fake-block-plugin.mjs` (logs every frame), `spikes/lab/caller-watch.mjs` (logs every `session.attach_all` `event_scope: all` event), `spikes/lab/caller-ident-init.lua` (lab config action), `spikes/lab/caller-responder.lua` (lab "hub" client).

### What the plugin receives

Every call path gives the plugin the same frame. Only `args` changes:

```json
{"type":"request","id":"control-N","method":"block.method",
 "payload":{"block_id":"<the plugin's own block ID>","method":"echo","args":{...}}}
```

| Path | `args` the plugin receives | Caller fields |
| --- | --- | --- |
| `rex api call block.method {session_id, block_id, method, args}` | the caller's `args` | none |
| `client.action {action:"<creator>.<action>", origin:"palette"}` (same path as the command palette) | `{}` (the action's fixed `args`) | none |
| `client.action` with `origin:"key"` (same path as a key binding) | `{}` | none |
| `rex do -s S -b B <creator>.<action>` | `{}` | none |
| Lua `rex.call("block.method", ...)` in `rex do` | the caller's `args` | none |
| Lua `rex.invoke("<creator>.<action>")` in `rex do -b B` | `{}` | none |

- The payload has exactly three keys: `block_id`, `method`, `args`. Rex drops `session_id`, the Rex block ID, `origin` and the calling client.
- `block_id` is the ID that the plugin returned from `block.create`, not Rex's block ID.
- An action that a block type declares has fixed `args`. A `client.action` request with `args` for it fails: `action ... has fixed arguments, so the request must not include args` (`bad_request`). `rex do <creator>.<action> k=v` fails: `takes no arguments`.
- `rex.block.call(block_id, method, args)` in `rex do` failed: `no handler for method "block:<uuid>.echo"` (`method_not_found`). Use `rex.call("block.method", ...)`.
- `rex -C <client> do <action>` is `client.perform` to a client. It does not reach a block plugin. A plugin is not a Rex client.

### A descriptor cannot ask for caller information

- The API reference for `block_creator.list` (saved in `docs/reference/rex-0.0.1792158932-g88fd41/help.json`) defines a method descriptor as `{description, input_schema, output_schema, actions}` and an action descriptor as `{name, title, description, category, keywords, args, repeats}`. Both have `additionalProperties: false`.
- Binary scan of the 1203 server (python, json struct tags): the types are `apitypes.BlockMethodDesc`, `apitypes.BlockActionDesc`, `apitypes.HelpBlockMethod`, `apitypes.BlockMethodInput`. Of 541 distinct json tags, only `caller`, `client`, `client_id`, `context`, `origin`, `principal`, `requester` relate to identity. Go stores tag strings once per binary, so the scan cannot show which struct owns a tag. `requester` and `principal` belong to `client.perform`. `origin` and `context` appear next to creator listing and event fields. `caller` appears next to libghostty callback types. No tag such as `want_caller` or `include_caller` exists. Unverified: that no hidden field exists. The schema says there is none.

### Events do not show which client called a block method

- With `session.attach_all` `event_scope: all`, a `block.method` or a block `client.action` produces **no event**. The watcher saw only `client_joined`, `client_left`, `client_changed`, `keymap_changed`, `client_performed` and session `client_connected` / `client_disconnected` / `client_connection_changed`.
- The plugin also receives `block.event` `client_connected` / `client_disconnected` with only `client_id` (no principal). For a short CLI call, the frames come in this order: `client_connected` (CLI client) → `block.method` → `client_disconnected` (same client), within 3 ms.
- This order gives a guess for one-shot CLI callers only. It fails for a long-lived client (the app, the hub, an agent's MCP shim), which connects once and calls many times. It also fails when two clients connect at the same time. Do not use it for identity.

### `args` can carry caller context, but anyone can forge it

- A plugin method can accept caller fields in `args`. A direct `block.method` with `args: {client_id: "client:forged", origin: "palette"}` reached the plugin in the same frame shape as an honest call. The plugin cannot tell them apart.

### Config file actions know the caller

- A Lua action in the config file (`rex.action` in `init.lua`, reloaded with `server.config_reload`) receives `ctx = {client_id, session_id, block_id, origin}`. The Rex docs list these fields too (`docs/reference/rex-docs-2026-10-16/llms-full.txt`, "The Run Function").
- Observed `origin` values: `api` from `rex do <action>` (the docs say `cli`), and `palette` from `client.action` with `origin: "palette"`.
- `ctx.client_id` is the calling connection. A Node client's two calls on one connection gave the same `ctx.client_id`.
- **`client.perform` from inside a config action is attested as the client that ran the action.** The lab action `ident` called `client.perform` on the "hub" client. The hub's `client_perform_requested` event had `requester.client_id` equal to `ctx.client_id`, and `requester.principal.pid` equal to the caller's pid (Node pid 93702; for the CLI, the `rex` process). `client_performed` (event scope `all`) shows the same requester.
- So the action's caller identity is attested through `client.perform`, and Rex fills it. A forged `args` value does not change it.
- Limit: a `rex.action` in a `rex do` script is not callable from other clients (see "Rex Lua runtime"). Only the config file (or a script installed with `client.set_script`, untested) gives a callable action. The config file is per account and is the user's file. LazyRex would have to install the action into it.
- A config action that calls `block.method` on the plugin, with `ctx.client_id` in `args`, does not help. The plugin receives the same frame as a forged direct call.

### Workarounds, ranked

1. **Use `client.perform`, not `block.method`, for calls that need identity (observed).** The hub (or the plugin process itself, with its own control connection, untested) declares client actions with `client.keymap`. Callers use `rex -C <hub-label> do <action>` or `client.perform`. Rex attests `requester {client_id, principal {uid, gid, pid}}`. The hub maps the pid to a pane by walking the process tree (see "Caller identity"). Races: the pid can exit before the hub walks the tree. A long-lived shim pid identifies the shim, not the agent. Requests queue one at a time per target client, so actions must answer fast.
2. **Command palette and key bindings:** a config action with `ctx.client_id` that forwards to the hub through `client.perform` (observed). This makes the app's palette and keys identity-carrying. The plugin's own block actions cannot do this.
3. **Nonce handshake (proposed, untested):** the plugin's method returns a one-use nonce. The caller sends the nonce to the hub through `client.perform`. The hub learns the attested requester and tells the plugin. Race: any same-uid process that reads the nonce first can redeem it. This adds a round trip and gives nothing that workaround 1 does not give.
4. **Event correlation (observed, not reliable):** see above. Use only as a hint for one-shot CLI calls.

All workarounds are attribution, not a security boundary: every process runs as the same uid.

### Native `rex do` route to the hub (observed on Rex 1203, lab, 2026-10-17)

Lab: `/tmp/rexlab-route`, started with `LAB_CONFIG=$PWD/spikes/lab/route-init.lua spikes/lab/server.sh /tmp/rexlab-route`. Probes: `spikes/lab/route-init.lua` (config action `lazyrex_send`), `spikes/lab/route-hub.mjs` (hub client labeled `lazyrex-messaging`). The callers ran in a lab pane (`com.superlogical.terminal.shell` with a script command).

Config action that forwards to the hub:

- `rex do lazyrex_send to=codex body=hello urgent=true --json` in a lab pane worked. KEY=VALUE args are typed against the action's `args` declaration: `urgent=true` arrived as a boolean.
- The hub's `client_perform_requested` had `requester.principal.pid` = the `rex` CLI process. Its parent was the pane's process (`ps`: pid 74892, ppid 74872 = pane script). So one parent step leads to the pane.
- `client_perform_requested` also carried `session_id` and `block_id` from the action's `ctx` (the caller's `REX_SESSION` / `REX_BLOCK`). A caller can set these, so trust only the pid.
- The action returned the hub's result. The CLI printed it on stdout: pretty JSON with `--json`, one compact JSON line without it. Exit status 0.
- Without a session (`rex do lazyrex_send ...` outside a pane, no `-s`), the action also worked. `ctx.session_id` is then absent.
- Argument checks: the CLI refuses an unknown argument (`no argument named "bogus"; the action takes body, to, urgent?`). A missing required argument was **not** refused by the config action. The hub's client action schema refused it inside `client.perform` (`arguments for "lazyrex.send": missing required property "body"`, returned as `internal`, status 500, through `client.action`).

Direct client action, for comparison: `rex -C lazyrex-messaging do lazyrex.send to=codex body=direct --json` from the pane also worked. The requester pid was again a child of the pane. The event had `origin: "cli"` and no `session_id` / `block_id`.

Discovery of a config action:

- `rex actions` lists it with its arguments (`lazyrex_send  LazyRex: Send message  body, to, urgent?`). `rex actions --json` gives `name`, `kind: "lua"`, `title`, `description`, `category`, `args` (a JSON Schema built from the Lua declaration, with `required` and `additionalProperties: false`) and `source` (`route-init.lua:4`).
- `client.keymap` returns the same entry in `actions`. The app builds its command palette from this list. Not checked in the app (lab has no app).
- `rex api list` does not list it (only API methods). `rex do --help` and `rex do lazyrex_send --help` show only the generic help. `rex client inspect` shows only a client's own actions.
- `rex.action` accepts `description`, `category`, `keywords`, `args` (a `{name = "type"}` table, `?` for optional, or a JSON Schema table) and `repeats` (Rex docs, "Defining an Action").

Client action descriptors:

- A `client.keymap` action with `kind: "client"` can declare `description` and `args` (a JSON Schema). `rex client inspect lazyrex-messaging` shows `ARGS body, to`. `--json` shows the full `description` and `args` schema. `rex -C lazyrex-messaging actions` shows the same list.

### Lost clients and reconnect tokens (observed on Rex 1203, lab, 2026-10-17)

- A hello may carry a client: `{"type":"hello","client":{"label":"lazyrex-messaging","kind":"other","name":"lazyrex-hub"}}`. The welcome then has `client: {id, reconnect_token}`. The binary's hello type is `{reconnect_token, client}`.
- **A client that is not attached to any session is forgotten at once.** After `kill -9`, the server logged `control connection lost, keeping client for reconnect`. But `client.list` and `target.resolve` no longer showed the client (checked at 0.06 s). A hello with its token (2.5 s and 17 s later) got a new client ID.
- **A client attached to a session (`session.attach`) becomes `lost`.** `client.list` shows `connection_state: "lost"` and `connection_lost_at`. `target.resolve` still resolves the label to it. `rex -C lazyrex-messaging do ...` fails with `"lazyrex-messaging" is waiting to reconnect`.
- **Resume works.** A new connection with `hello.reconnect_token` = the last token, 8 s after the loss, got `client: {id: <same ID>, reconnect_token: <new token>, resumed: true}`. `-C lazyrex-messaging` worked again. Each welcome gives a new token, so store the latest one. `client.list` still showed the old pid as `principal.pid` after the resume.
- Grace period: a lost attached client disappeared 61 s after the kill (polled every 2 s). The binary has `ReconnectGraceMillis`. The welcome frame did not include it.
- During the grace period, a new client with the same label (no token) made the label ambiguous: `target.resolve` returned `ambiguous` with both clients, and `-C lazyrex-messaging` failed with `client target "lazyrex-messaging" is ambiguous`. After the old client expired, `-C` resolved to the new client.
- So a hub keeps one stable client ID and one unambiguous label if it: attaches to a session, persists the latest `reconnect_token`, and reconnects with it within about 60 s. A hub that restarts later must wait for the old client to expire, or callers see an ambiguous label for up to about 60 s. Untested: whether a resumed client keeps its `client.keymap` actions and session attachment without sending them again (the probe sent them again), and whether a token works after a server restart (`instance_id` changes; probably not).

### Window labels (observed on Rex 1203, 2026-10-17)

- `window_label_changed` payload: `{window_id, label}` (lab probe). The event names one window.
- A window that nobody named has the generated label `"Window " +` the last 8 characters of its window ID (for example `window:…cc5176a0a502` → `Window 76a0a502`). Seen on two windows of the real server (read-only `session.view`). `session.set_window_label` with an empty label restores it. No field marks a label as generated, so LazyRex compares against this form.
- A session holds at most 64 windows. `session.new_window` then fails with `conflict: limit reached: the session already has its maximum of 64 windows; close one before opening another` (lab: a messaging app session with one tab per thread hit it).

### Request for superlogical

Add the calling client to `block.method` requests sent to block plugins, in the same shape as `client.perform`'s `requester {client_id, principal}`, plus `origin` and the Rex `session_id`. Or let a method descriptor ask for it (for example `"caller": true`).

## Command palette entries and forms (Rex 1203, 2026-10-17)

Sources: lab `/tmp/rexlab-palette` (`LAB_CONFIG=spikes/lab/palette-init.lua`, `fake-block-plugin.mjs` as `probe`, `route-hub.mjs`); probes `spikes/lab/palette-probe.mjs` and `spikes/lab/palette-form-probe.mjs`; the Rex docs (`docs/reference/rex-docs-2026-10-16/llms-full.txt`); a string scan of `/Applications/Rex Beta.app/Contents/MacOS/Rex` (app 0.1.0 (1203)); one read-only `rex client ls` / `rex client inspect <app>` on the real server. The Rex app was not connected to a lab server. So every statement about what the app shows is **inferred**, not observed.

### Which actions the palette can show

- Config actions (`rex.action` in `init.lua`): **documented** ("From the command palette. Search for the action's title"). `title`, `description`, `category` and `keywords` are palette fields.
- `client.keymap` returns the actions the server offers to a client. In the lab, an app-like client (hello `kind: "app"`, `session.bootstrap`, then `client.keymap {}`) got `kind: "lua"` (the config actions) and `kind: "block"` (terminal block actions and the plugin's `dev.lazyrex.probe.echo`). It did **not** get the `kind: "client"` actions of other clients (the lab hub's `lazyrex.send`). Observed.
- Inferred: the app's palette is its own actions plus this keymap list. So another client's client actions do not appear. The app binary cannot confirm this either way: Swift stores strings of 15 bytes or fewer inline in code, so method names such as `client.keymap` or `client.perform` do not appear as strings. Check by hand: open the palette in the app and search for an action that the official hub declares.
- Block plugin actions: a block action runs on the focused block and fails with `conflict` when that block is of another type (`client.action` reference). So a plugin action is useful only when the plugin's block has focus. It cannot be a global palette entry.

### LazyRex palette file (observed on Rex 1203, lab `/tmp/rexlab-pal`, 2026-10-17)

- `init.lua` can load a file beside it with `require` (Rex docs, "Splitting Configuration"). `pcall(require, "lazyrex_palette")` loads the hub's generated file and does nothing when the file is missing.
- `rex config reload` applies a changed file. The hub runs it after each content change. With an error in the file, reload prints `error <file>:<line>: …` and exits nonzero.
- `rex.action` takes `keywords` as one string. A table fails the load: `rex.action: field "keywords" must be a string, got table`.
- A config action's `rex.call("client.perform", …)` reaches a client action. The performer received `args.ctx = {session_id, block_id, client_id, origin: "palette"}`. The `requester` was the client that ran `client.action` (`requester.client_id == ctx.client_id`), not a server principal.
- `rex.client.queue("session.select", {session_id, window_id})` inside that action came back in the `client.action` response as `client_actions: [{action: "session.select", args: …}]`.
- `session.new_layer` with a terminal block and `exit.on_completion: true`: a renderer exit (Ctrl+Q) closes the layer, and Rex emits `block_closed`.

### Floating layers in the app (Rex app 1203, 2026-10-17)

- Observed on the real app (the maintainer's screenshot): a floating layer that runs a terminal program shows the tab below through every cell that has the default background. Cells with an explicit dark background (SGR 48) looked opaque in that screenshot. The two observations below disprove the first inference (only the default background is transparent).
- Observed on the real app (the maintainer, 2026-10-17): a form looked opaque in a fixed dark color, then turned transparent after it painted every cell with the exact theme background color (from the OSC 11 reply). The inference that only the exact theme color is transparent was disproved by the next observation. Probably the translucency is easier to see in a color near the theme background.
- Observed on the real app (the maintainer's second screenshot, 2026-10-17): with every cell painted in a raised surface color, the pane below still showed through the whole floating layer. Inferred: the app draws floating layers translucent, whatever the cell colors. Rex 1203 has no API or config option for layer opacity (checked: the API reference and the docs on 2026-10-17). LazyRex forms therefore open as a split on the right of the source pane (`session.new_split`), not as a floating layer.
- Observed on the real app, build 1347 (the user, 2026-10-22, on the user's own server at the user's request): a floating layer (`session.new_layer`, bounds 0.15/0.15/0.7/0.6, `focus: true`) is OPAQUE and looks right. The grey in that test was the test's own color 238. So the translucency above was build 1203 only. The hub can open forms and panels as layers (`ui_slot`, docs/hub.md, "Palette forms and thread menus").
- Observed on Rex 0.0.1792158932-g88fd41 (lab, 2026-10-22; probe `lab/probes/layer-close.lua`): `block.close` of the only block of a floating layer removes the layer, and the window's focus goes back to the block under it. A layer block's exit with `exit.on_completion: true` does the same. With two layers, the close of the front one gives focus to the other layer's block. `session.new_layer` answers `{layer_id, block_ids, revision}`, with no `anchor_block_id`.

### What a palette run carries

- `client.action` takes `origin` (`palette`, `key`, `api`) and `session_id` / `block_id` from the calling client. With `session_id` and no `block_id`, Rex used the session's focused block as `ctx.block_id`. Without `session_id`, `ctx` had only `client_id` and `origin` (lab).
- Inferred: the app sends the session that it shows, because block actions from the palette must run on the pane the user is in. Not observed.
- `client.perform` passes `origin`, `session_id` and `block_id` through unchecked. The lab hub received `origin: "palette"`, the session and the block, plus the attested `requester` with `info.kind: "app"`.

### Arguments and forms

- **The server does not enforce a config action's required args on `client.action`.** `lazyrex_form` declares `need = "string"` (required). A palette-style call with no `args` ran with `args = {}`. Only the CLI checks args (it refuses unknown names). So a palette entry can always run, and the action must handle missing args itself.
- No documented way exists for a palette entry to prompt for args. The docs say only "Search for the action's title".
- The app has fixed built-in pickers and pages (`GoToDirectoryCommandPalettePage`, `HostCommandPalettePage`, `ModeCommandPalettePage`, `CustomThemeCommandPalettePage`, `ShortcutsCommandPalettePage`, `UpdateChannelCommandPalettePage`, `CommandPalettePrompt`). The real app's declared actions (read-only `client inspect`) include pickers only for built-in things: `session.switch` ("Change Session…"), `client.theme.change` without `name` ("Omit to open the theme picker"), `pane.go_to_directory`, `pane.move_to_tab`, `client.tab.rename`. **No app action opens a generic prompt, choice list or form for a client.**

### Which session and tab the user is viewing

- The server's `session.view.active_window_id` is per session and shared by all clients. It does not follow the app: the app keeps its own tab (see "Driving the Rex app").
- `client.list` lists the sessions a client attached (`session_ids`). The real app attached 4 sessions at once. So it does not show the viewed session.
- No app action returns the current session or tab (read-only `client inspect` of the real app).
- The best signal is a palette run: a config action's `ctx.session_id` / `ctx.block_id`, sent by the app (inferred). Focus reporting (DECSET 1004) tells a pane program that its own pane got focus. It does not tell the hub about other panes.

### Floating form layer

- `session.new_layer {session_id, window_id, bounds, layout, focus: true}` created a frontmost floating block. In the active window, it became the window's `focused_block_id`, and the active window did not change. In a non-active window, `focus: true` also made that window the session's active window. Observed (lab).
- This changes only server state. Earlier observation on the real app: server focus does not move the app's tab, and the app's `session.select {session_id, window_id}` shows the tab with the server's focused block as the app's focused pane.
- **Route that works in the lab:** a config action run from the palette (`spikes/lab/palette-init.lua`, `lazyrex_form`) finds the window of `ctx.block_id`, opens a focused floating layer there, and calls `rex.client.queue("session.select", {session_id, window_id})`. The `client.action` response returned `client_actions: [{action: "session.select", args: {...}}]`. The calling client (the app) performs these after the action. The docs say that `rex.client.queue` fails when the action runs from the CLI and no client can be found. They also say that the app accepts actions from the CLI only with Remote Control on. A palette run comes from the app itself. Inferred: it does not need Remote Control. Not observed in the app.
- Run from the CLI without a session, the same action failed, because `ctx` had no session.

## Hiding and grouping

- No session hide or archive exists. `session.list` items carry only `session_id` and `label`.
- Hide a session: detach every block. Rex removes each empty window. The session has 0 windows, processes keep running, and **the app sidebar still shows the session header with nothing under it**.
- Attachment (observed on Rex 1203, lab): on a session the connection has not attached, `session.list_blocks`, `com.superlogical.terminal.process` and `block.close` fail with "stream is not attached to session …". `com.superlogical.terminal.program_status` fails the same way (Rex 0.0.1792158932-g88fd41; see "Program status needs an attached session").
- OSC 11 (observed on Rex 1203, lab): a terminal answers a background color query only after a client has sent a theme (`com.superlogical.terminal.set_theme`). A block that no client has themed gives no answer, placed or detached. After `set_theme` the terminal answers with that background, also while detached. So a program that queries once at start, in a pane the app has not shown yet, cannot learn the app's theme.
- Destroy order (observed on Rex 1203, lab, `destroy-order.mjs` probe): `session.destroy` emits `block_closed` for each block (placed and detached), then `session_destroyed`. When a client handles `block_closed`, `session.list_blocks` already answers `session … not found`. So a `block_closed` handler can tell a session loss from a single close.
- Unhide: `new_window` with a placeholder block, `move_block` the detached blocks next to it, then close the placeholder. Verified: the block returns with its process running.
- Detaching every block of one window removes that tab completely.
- Checked 2026-10-16 on Rex 1203 for a session hide: server API (`session.*`), app actions, CLI `rex session`, the 21 docs pages and the API reference. None has one. `session.detach` only ends a control connection's attachment; the screen does not change.
- The app's Activity filter (eye icon, app action `client.sidebar.activity.toggle`, undocumented) shows only placed tabs whose programs report OSC 7501 status. Observed on Rex 1203, real app: with the filter on, an empty session disappeared, and a session whose only block was detached and reported `working` also stayed hidden. The toggle reports no state. The maintainer rejected building app mode on it (too much of a workaround).

## The macOS app

- It shows terminal blocks only. Other blocks show "Unsupported block". There is no WebView, so LazyRex cannot draw custom UI in the app. Evidence: see "Custom block types in the app" below. The app string and the server behavior are observed. The app's drawing of a plugin block has not been seen on screen.
- It connects to one host at a time (`client.host.switch`).
- Terminal features (the maintainer, 2026-10-16): Rex passes through everything Ghostty supports, including mouse reporting. So TUIs can rely on mouse input without a Rex test. By the same rule, the kitty graphics protocol should work (not tested).
- Other clients can drive its view through its actions, when Remote Control is on. See "Driving the Rex app".
- It has a `session.open_on_web` action ("Open Session on Web…"). It probably opens the hosted Rex web client. See "Browser access".
- Rex client Lua (`rex do`, `init.lua`) is gopher-lua (Lua 5.1, pure Go) running in the client. It has no database, sockets or MCP library built in. A `rex do` script can serve RPC through `client.perform` (see "Rex Lua runtime"). Whether it can host LazyRex plugins in practice is not yet tested.

### Custom block types in the app (Rex 1203, 2026-10-17)

Sources: a string scan of `/Applications/Rex Beta.app/Contents/MacOS/Rex` (app 0.1.0 (1203), read-only); the Rex docs (`docs/reference/rex-docs-2026-10-16`); lab `/tmp/rexlab-block` with `spikes/lab/wrap-block-plugin.mjs` and `spikes/lab/block-render-probe.mjs`.

App binary (observed, static):

- The app has one block configuration file, `RexMacOS/BlockUI.Configuration+Terminal.swift`. The other `BlockUI` files are terminal files (`TerminalContentView`, `TerminalFileHandoff`, `TerminalRendererResources`, `TerminalSearchHighlights`, `TerminalSearchModel`, `TerminalSurfaceSupport`) and `BlockUI.ContentView.swift`.
- Next to `BlockUI.ContentView.swift`, the app has three fallback texts: "The content environment does not provide the capabilities required by this block.", "Renderer Unavailable" and "Unsupported block", with the symbol `questionmark.square.dashed`.
- The only data protocol in the app is `rex.data.terminal.v1` (next to `com.superlogical.terminal` and `com.superlogical.terminal.shell`). The binary has no `WKWebView` or `WebView` string.
- Inferred: the app chooses a renderer by block type, and it has a renderer only for `com.superlogical.terminal`. A plugin block gets the "Unsupported block" view. Not seen on screen.

Server (observed, lab):

- `session.describe_block` for a plugin block gives `api.methods` only. A terminal block gives `api.events` and `api.methods`.
- `data.connect` for a plugin block fails: `not_found`, "block data provider not found", with and without `subprotocol: "rex.data.terminal.v1"`. The same call for a terminal block returns a ticket. The server's plugin code (`blockremote`) has methods, events, an emitter and close, but no data or stream symbols. So a plugin cannot serve the terminal data protocol, and the app has nothing to draw.
- A terminal action on a plugin block fails: `conflict`, "action "com.superlogical.terminal.write" is for com.superlogical.terminal blocks, but block … is a dev.lazyrex.probe block".
- **A plugin cannot wrap a terminal.** `wrap-block-plugin.mjs` returned the Rex ID of a live terminal block from `block.create`. Rex created a new block with its own Rex ID and `creator_name: "dev.lazyrex.probe"`. It had only the plugin's methods, no data provider, and `write` failed with `method_not_found`. `block.method echo` on it reached the plugin with the terminal's ID as `block_id`. So Rex treats the ID that `block.create` returns as an opaque handle for the plugin.
- A plugin can create a terminal block as a normal client (`session.new_block`, from its own control connection). That terminal is a separate `com.superlogical.terminal` block. Its palette actions are the terminal's, not the plugin's. Not tested, because it is ordinary client behavior.

Safe in-app check (for the maintainer, if wanted; not run):

1. Start a TCP lab with the probe plugin. Do not use port 47900 if something else uses it.
   ```sh
   L=/tmp/rexlab-app; mkdir -p $L/blocks
   printf '#!/bin/sh\nLAB_DIR=%s exec node %s\n' $L "$PWD/spikes/lab/fake-block-plugin.mjs" > $L/blocks/probe; chmod +x $L/blocks/probe
   LAB_EXTRA_ARGS='--extra-listen http://127.0.0.1:47900 --allow-anonymous' spikes/lab/server.sh $L
   ```
2. In another terminal, make a session with a terminal and the plugin block side by side:
   ```sh
   env -u REX_SESSION -u REX_BLOCK -u REX_SERVER rex -S http://127.0.0.1:47900 --autostart=false api call session.create \
     '{"label":"custom-block-check","initial_windows":[{"layout":{"split":{"direction":"horizontal","ratio":0.5,"before":{"block":{"flavor":"com.superlogical.terminal.shell"}},"after":{"block":{"flavor":"dev.lazyrex.probe.service"}}}}}]}'
   ```
   Observed in the lab: the TCP listener admitted the CLI as `anonymous`, and this call created the session.
3. In the app: palette, "Add Remote Host…", endpoint `http://127.0.0.1:47900`, label `lab`. This writes the host to the maintainer's home server's host list (`rex hosts`). Then "Switch Host" to `lab`, and open `custom-block-check`. Look at the right pane.
4. Switch back: "Switch Host" to the local host. Remove the lab host: "Remove Host…" in the app, or `rex hosts remove lab`. Stop the lab server with Ctrl-C in step 1.
5. Untested: whether the app accepts an `http://` anonymous endpoint as a host. If it refuses, the check is not possible without a tailnet.

## Tab status: program status, glyphs and notices (Rex 1203, 2026-10-17)

Sources: Rex docs "Program Status Protocol (OSC 7501)" (`docs/reference/rex-docs-2026-10-16/llms-full.txt`), labeled **documented**; lab `/tmp/rexlab-status` with `spikes/lab/status-emit.sh` and `spikes/lab/status-probe.sh`, labeled **observed (lab)**; read-only block methods on this agent's own Claude pane on the real server, labeled **observed (real, read-only)**; a read-only scan of the app bundle (`/Applications/Rex Beta.app`, 0.1.0 (1203)), labeled **inferred** where it explains app behavior. Nobody checked the app's hover card for these tests.

### Where the hover card's data comes from

| Card element | Most likely source | Label |
| --- | --- | --- |
| Program icon | The status record's `app` / `effective_app`, or the foreground program, matched against a fixed table in the app | inferred |
| Tab name | The window label | inferred |
| Status glyph (spinner, symbols) | The record's `state` (and `kind` for `blocked`). The docs say: "Tab headers for unfocused tabs show an in-progress spinner and a variety symbols for blocked states." | documented |
| Task title | The terminal title (OSC 0/2). The real Claude pane had title "Block.method caller identity" and a record without `title` or `msg`. | observed (real, read-only); the mapping is inferred |
| State ("Done") | The record's `state` | inferred |
| Working directory | The process `cwd` (`com.superlogical.terminal.process`). The Claude pane had no OSC 7 `pwd`. | observed (real, read-only); the mapping is inferred |

The real Claude pane (Claude Code 2.1.302) reported one root record: `{"state":"working","app":"claude-code","effective_app":"claude-code","updated_at":…,"owner":{"pid":…,"name":"2.1.302"}}`. Its foreground process name is `2.1.302` (a versioned binary), and `argv0` is `…/bin/claude`. Observed (real, read-only).

### Record fields (observed, lab)

- `com.superlogical.terminal.program_status` returns `{records:[…]}`. Each record has `id` (absent for the root record), `state`, `kind`, `progress`, `app`, `effective_app`, `title` and `msg` (decoded text), `updated_at` and `owner {pid, name}`.
- A child record (`id=form/new`) without `app` got `effective_app` from the root record. When the root record's `app` changed to `claude-code`, the child's `effective_app` changed too.
- Each report replaces its record. `state=clear` removed all records and sent `program_status_removed {ids:["","form/new"], reason:"clear"}`.
- Each change sends a `block_event` `program_status_changed {id, record}` on the session event stream.

### Icons and glyphs

- The app bundle has a fixed asset catalog, `SuperBlockIcons.bundle`, with 96 `block-glyph-*` icons: for example `claude`, `codex`, `gemini`, `cursor`, `git`, `gitchanges`, `docker`, `neovim`, `vim`, `python`, `rust`, `go`, `node`, `bun`, `terraform`, `rex`, `terminal`, `log`, `sleep` and `placeholder`. Observed (static).
- The app code has a table of short keys with display names (`claude` → "Claude", `codex` → "Codex", `opencode` → "OpenCode", "Neovim", "Git Changes", …). Observed (static). Inferred: the app picks a glyph from this table by the record's `app` or by the foreground program. There is no entry for `lazyrex`.
- **No protocol field sets an icon or a glyph image.** OSC 7501 has no icon key, and the docs say the protocol "leaves presentation to the terminal". A program can choose only an `app` value. Inferred: an `app` that is in the table (for example `rex`) borrows that glyph, and an unknown `app` shows a generic glyph. Not checked in the app.
- From the Rex team (the maintainer relayed it, 2026-10-18; not checked in the app): the app picks the glyph from the program's process name today. Ways to get a glyph: name the binary after a table key (for example `gitchanges`), run it as `exec -a gitchanges ./program`, wrap it in a parent process with that name, or set `process.title` in Node. The Rex team calls this provisional ("a bit wacky"), because programs are distributed, wrapped and packaged in many ways. They plan a formal protocol for it. The same Rex team member previews glyphs with a small tool that copies a binary that waits on stdin to a file named after the key, runs it, then deletes it. The asset catalog (`assetutil --info` on `SuperBlockIcons_SuperBlockIcons.bundle/Contents/Resources/Assets.car`, Rex Beta 0.1.0 (1203)) holds 96 `block-glyph-*` names, for example `git`, `gitchanges`, `jj`, `log`, `inspector`, `terminal`, `rex`, `tailscale`, `cloudflare`, `sleep`. Unknown: whether the matching key is always the glyph name (a process `nvim` may map to `neovim`).

### Other signals (observed, lab)

| Sequence | What Rex does |
| --- | --- |
| OSC 9;4 (`ESC ] 9;4;1;55`) | A `progress_report` block event `{state:"set", progress:55}`. It does **not** create a program status record, with or without an earlier OSC 7501 report. |
| OSC 9 (`ESC ] 9;text`) | `desktop_notification {title:"", body}` |
| OSC 777 (`ESC ] 777;notify;T;B`) | `desktop_notification {title, body}` |
| OSC 99 | No event |
| BEL | `bell {}` |
| OSC 2 | `title_changed {title, owner}`; `com.superlogical.terminal.title` reads it |
| OSC 7 | `pwd_changed {pwd, owner}`; `com.superlogical.terminal.pwd` reads it |

How the app shows `progress_report`, `desktop_notification` and `bell` was not checked. The docs say that sessions with `done` or `blocked` records get an indicator in the session picker (documented).

### Proposal for LazyRex tabs (proposed)

- The renderer of a tab is a terminal program, so it reports status with OSC 7501 on its own pane. No new Rex feature is needed.
- The hub lets a plugin set per tab: `status {state, kind?, progress?, title?, msg?}`, and `title` (the tab's terminal title, OSC 2). The renderer writes the root record with `app=lazyrex` (or a borrowed glyph key, after an in-app check) and repeats every key in every report.
- A view with several items (for example a thread list) can use child records, `id=thread/<id>`, with `title` and `msg` per item. The docs say a terminal "MAY use it for grouping". How the app shows child records is unknown.
- Map hub states to protocol states: unread or ready result → `done`; waiting for the user (a form, a question) → `blocked` with `kind=question`; agent at work → `working`; failed → `error`; nothing pending → `idle` or `clear`.
- Do not offer a plugin an icon field. The app has no way to take one. Offer `app` only as a fixed LazyRex value.
- Use OSC 9 or OSC 777 for a one-time notice only if the maintainer wants desktop notifications. Rate-limit them (the docs say a terminal SHOULD rate-limit them; LazyRex should too).

## Block close and child exit events

Observed on Rex 0.0.1792088193-g7a93ec, lab, 2026-10-16. Rex documents session events at https://www.superlogical.com/rex/docs/reference/events (`block_created`, `block_closed`, `window_closed`, ...). The saved `llms.txt` describes methods only, not events.

- `block.close` on a pane whose child is alive: the session stream has `block_closed {block_id}`, then `window_closed` and `session_view_changed`. Rex ends the child process. There is **no** `child_exited` block event.
- The child exits in a pane with `--keep-open` (`exit.on_completion=false`): there is a `child_exited` block event and no `block_closed`.
- So `block_closed` means the pane closed, and `child_exited` means the process ended in a pane that stays open. For a keep-open pane, a hub can tell a pane close from a process exit without timing.
- `block_closed` and `window_closed` carry no actor or cause (events page). A close by the user, by the hub, by any other client, or (untested) by a child exit without keep-open looks the same. To know that the user closed a pane, the hub must rule out its own closes (correlate its operations) and use keep-open for its panes. (Correction from Codex's review, 2026-10-16.)
- Also seen, not in the events page: `block_owner_changed {block_id}` on block creation.

### Lua helper availability (observed 2026-10-16)

Observed on Rex `0.0.1792088193-g7a93ec` in an isolated server:
`type(rex.json)`, `type(rex.exec)`, `type(rex.after)`, and `type(rex.every)` return `nil`.
`type(io.popen)` and `type(os.execute)` return `function`.
This probe checks availability, not execution behavior.
The current online Lua reference lists the missing helpers and says the two standard process functions are disabled.
Use the installed runtime as the compatibility boundary.
Audit evidence: `/tmp/rexlab-codex-browser/reinvention-sources/`.

### Focus reporting reaches panes (observed 2026-10-16, Rex 1203, real app)

A program in a pane that enables focus reporting (DECSET 1004) gets focus-in (`ESC [ I`) and focus-out (`ESC [ O`) from the Rex macOS app when the user moves between panes and tabs. Observed with the spike 3 renderer (Bubble Tea v2 focus messages): the maintainer's clicks into thread tabs produced 12 `hub.view.seen` calls, and only those threads were marked read. A background tab gets no focus-in. Not checked: whether enabling 1004 reports the current state at once (xterm does not).

### Lua swap assignment is wrong (observed 2026-10-16, Rex 1203, lab)

Observed on Rex `0.0.1792158932-g88fd41` with `rex do` in an isolated server:
`a, b = b, a` gives both variables the old value of `b`. The result was the same for function parameters and for locals.
Standard Lua evaluates all right-hand values before it assigns, so `a, b` must swap.
Workaround: swap through a temporary local (`local t = a; a = b; b = t`).
Impact found: the messaging plugin wrote some thread records under `thread/<x><x>` (one agent twice) when the two IDs needed a swap.
Probe: `spikes/lab/lua-swap-probe.lua`.
Still wrong on Rex 0.0.1792158932-g88fd41 (lab, 2026-10-19). The rule: in a plain multiple assignment to existing variables, a later value that reads an earlier target sees the new value. `x, y = y, x + 10` with x = 1, y = 2 gives 2, 12 (Lua: 2, 11). A `local` declaration (`local a, b = b, a`) and table fields (`t[1], t[2] = t[2], t[1]`) are correct, and the values are evaluated left to right. A scan of the LazyRex plugin repositories (2026-10-19) found this pattern only in the probe and in two test helpers, which now use a temporary.

### move_block stays inside one session (observed 2026-10-16, Rex 1203, lab)

- `session.move_block` moves a detached block only beside an anchor in the same session. Cross-session moves fail with `404 not_found` whichever session the call names: "block … not found in any window". The same move beside a block of its own session succeeds.
- `session.new_split` with `anchor_block_id` = a side pane, then `block.close` of that pane, gives the new pane exactly the old pane's area (observed on Rex 0.0.1792158932-g88fd41, lab, `spikes/lab/slot-replace-probe.mjs`): a side pane resized to [0.3,0,0.7,1] was replaced by a pane at [0.3,0,0.7,1]. A side pane opened with `ratio: 0.55` gets 45% of the width (the ratio is the before side).
- So a LazyRex view cannot mount a block from another session. A "hub frame" that swaps blocks can only swap blocks of its own session.

### Detach and restore preserve view processes (observed 2026-10-16, Rex 1203, lab)

Observed on Rex `0.0.1792158932-g88fd41` in an isolated server with five LazyRex messaging views:

1. Attach the control connection with `session.attach`.
2. Switch between two thread tabs with `session.focus_window`. Ten switches gave the requested `session.view.active_window_id`.
3. Enter a draft through `com.superlogical.terminal.write`. Wait for the messaging plugin to save the draft in SQLite.
4. Call `session.detach_block` for all five view blocks. Rex removes the empty windows. `session.view` omits `windows` when no windows remain.
5. For each view, call `session.new_window` with a temporary shell block. Call `session.move_block` with the original block and the temporary block as `anchor_block_id`. Close the temporary block with `block.close`.
6. Read each renderer PID with `com.superlogical.terminal.process`. All five PIDs match their values before detachment.

The original block IDs survive. Each restored view has a new window ID. The saved draft text and revision remain unchanged. Terminal capture after restore contains the draft.

The hub ran during detachment. Its tab ordering attempted to use the removed window IDs and received `not_found`. The test stopped the hub before restoration. After restoration, the hub database still held all five old window IDs. Thus, this test proves the Rex methods and process preservation. It does not prove hub mount recovery. The hub must handle detached views and update window IDs before it can provide that behavior.

No Rex app connected to this lab. The test proves server focus, not app selection.

### A detached split layout can be rebuilt exactly (observed 2026-10-16, Rex 1203, lab)

- `session.view` reports each window's tiled layout as a tree: `split {direction, ratio, before, after}` or a leaf `{block_id}`, each node with a normalized `rect`.
- `session.new_window` takes the same tree as input, with `before` and `after` children (`first`/`second` is refused: "layout split before child is required").
- `session.move_block` takes `direction`, `side` (`before`/`after`) and `ratio`.
- Proof: a window with three blocks (horizontal 0.3, then vertical 0.5 on the right) was detached completely, and Rex removed the window. Rebuild: a new window with a placeholder block; `move_block` of the first leaf beside the placeholder; `block.close` of the placeholder; `move_block` of the second leaf beside the first (`horizontal`, `after`, ratio 0.3); `move_block` of the third leaf beside the second (`vertical`, `after`, ratio 0.5). `session.view` then showed the same tree, the same block IDs and the same ratios.
- To rebuild a tree in general, place the first leaf of the tree, then add each `after` subtree beside the last leaf of its `before` subtree, outermost split first. Untested beyond this case: deeper trees, floating layers, and the window's focused block.

### A whole session layout can be rebuilt exactly (observed 2026-10-16, Rex 1203, lab)

Template create check (observed 2026-10-17 on Rex `0.0.1792158932-g88fd41`,
`spikes/03-ui-framework/tests/templates-layout-lab.mjs`): one `session.new_window`
created four leaves with splits nested on both sides. Ratios and depth-first block
IDs matched the requested tree. Each leaf kept its label and its own `options.cwd`
(`/tmp` resolved to `/private/tmp`). An invalid flavor in the second leaf left no
new blocks or window. This checks that rejection case. It does not prove crash
atomicity or the absence of transient processes during every failure.

- `session.view` gives each window `label`, `focused_block_id`, optional `zoomed_block_id`, and `layers[]`. The first layer is `kind: tiled` with bounds `[0,0,1,1]`. Floating layers follow, back to front, each with `bounds` and its own layout tree. An empty tiled layer has no `layout`.
- Detaching every placed block removes every window. The session stays alive with no windows (`windows` is absent from the view).
- Rebuild of one window, in this order:
  1. `session.new_window` with the window label, one placeholder leaf, and `focus: false`.
  2. For each floating layer, back to front: `move_block` its first leaf beside the placeholder, `float_block` it with the recorded `bounds` (a new frontmost layer), then expand the layer tree.
  3. For the tiled layer: `move_block` its first leaf beside the placeholder, `block.close` the placeholder, then expand the tree. With an empty tiled layer, close the placeholder only.
  4. `focus_block` the recorded focused block, then `zoom_block` the zoomed block (`zoom: true`). Zoom comes last because a focus change clears zoom.
- Expand a tree: the region of each split is held by its first leaf. For each split, `move_block` the first leaf of `after` beside the first leaf of `before`, with the split's `direction`, `side: after` and `ratio`. Then expand `before` and `after`. `ratio` is the before share.
- After all windows: `focus_block` the focused block of the recorded active window.
- Proof (`spikes/lab/layout-rebuild.mjs`): 17 blocks in 5 windows. Cases: splits nested on the `after` side and on the `before` side, two floating layers (one with an inner split), a window with only floating layers, zoom, per-window focus, the active window, window order and labels. The view after the rebuild matched the capture exactly (block IDs, ratios, bounds, stacking order). Rebuild time: 170 ms.
- Window, layer and split IDs change. Block IDs do not change.
- Restoring next to windows created after the capture: the hub app-mode test (`spikes/03-ui-framework/tests/appmode.mjs`) restored recorded tabs first with `session.move_window`, and a tab the user made while hidden stayed after them.
- `session.new_window` and `session.move_block` need the control connection attached to the session (`session.attach`). Without it: `conflict`, "stream is not attached to session".
- A session with no windows has no `windows` field in `session.view`.
- Error codes for a missing target (observed 2026-10-16, Rex 1203, lab): a missing session gives `session_not_found` (`session.attach`, `session.view`, `session.list_blocks`). A missing block (`block.close`, `session.detach_block`) or window (`session.focus_window`, `session.move_window`) gives `not_found`. Check both codes for a session.

### A lost new_window answer leaves panes that the hub cannot prove (observed 2026-10-17, Rex 1203, lab)

- `spikes/03-ui-framework/tests/templates-lab.mjs` with `TEMPLATES_RESTART=1`: the hub was killed (SIGKILL) right after a template start was admitted. Rex had created the whole tab. After the restart, each leaf had exactly one pane with its `lazyrex-spawn:<id>` label, and the hub kept each accessory leaf `sending` ("outcome unknown: 1 candidate pane(s)"). A label is only a hint, so this is by design (decision, 2026-10-17: v1 keeps it; see roadmap, "Idempotent create operations").
- A process proof through the environment does not work on macOS for the main case. Observed on macOS 27.0.1 (26A434), own user: `ps eww -o command= -p <pid>` and `ps -E` showed `LAZYREX_SPAWN=…` for a Homebrew `node` process, and no environment for `/bin/sleep` and `/bin/zsh -c …`. Inferred: macOS hides the environment of Apple platform binaries. The built-in `shell` profile runs `$SHELL -l` (`/bin/zsh`).

### A detached pane still works for agents (observed 2026-10-16, Rex 1203, lab)

- `com.superlogical.terminal.write` to a detached block reaches its program. A notice line arrived in a `read` loop.
- The program keeps running: a counter loop advanced while detached.
- `com.superlogical.terminal.program_status` answers for a detached block, with the record set before the detach.
- So hub notices to agents (`agents.go`) and agent readiness work while the agent's pane is detached. Probe: `spikes/lab/detached-pane.mjs`.
- Shell block options go in `options` (`command`, `login`, `exit`). With the command in `args` instead, the command did not run (what ran instead is untested).

### Text decoration in the app (observed 2026-10-16, Rex 1203)

- Overline (SGR 53): the server keeps it. `rex capture --format=vt` returns `ESC[53m` (lab). The macOS app does not draw it (the maintainer's screenshot of a `less -R` pane).
- Underline (SGR 4): the app draws it at the font's underline position, above the cell bottom. A full-height glyph such as `▏` extends below the underline.
- A 2-column x 1-row filled block is square in the app (default font).
- Do not build outlines from overline or underline. Use full cells or block elements.

### Label resolution includes lost clients (observed 2026-10-16)

Observed on Rex `0.0.1792088193-g7a93ec` during an isolated hub recovery test:
`target.resolve` returned an ambiguous label for the old hub and its replacement after the old hub exited.
`client.list` identified the old hub connection as `lost`.
For live-service discovery, inspect each candidate with `client.inspect` and check `client.connection_state`.
Accept exactly one `connected` candidate. Refuse two connected candidates. Retry when none remain.
Native CLI label targeting can have the same ambiguity. Use the discovered full client ID after a restart.
On Rex 1203 (lab, 2026-10-17) this happens only for a client attached to a session, for about 60 s, and a hub can avoid it with a reconnect token. See "Lost clients and reconnect tokens".
Evidence: `/tmp/rexlab-codex-rework/recovery-tVAYVz/`.

## Seen but not working

Features that we found in Rex (binary, docs or API) that did not function or had no effect in our tests. Each entry: what, where seen, what we tried, Rex version.

| What | Where seen | What we tried and the result | Rex version |
| --- | --- | --- | --- |
| `sdk_version` and `features` in a block plugin's fd-3 JSON | Rex binary (`json:"sdk_version,omitzero"`, `json:"features,omitzero"`); startup log line | `sdk_version:"9.9.9-lazyrex"`, `features:["block_events","lazyrex.test"]`. Shown only in the startup log. No API shows them. No behavior change, and no feature name compared in disassembly. A non-string item makes the start fail. See "Plugin handshake fields". | 1203, lab |
| Plugin `block_action` of type `api` (`{"type":"api","payload":{target, method, id, args, caller?}}`) | Rex SDK `sdk/block/action` (`json_action.go`) | Rex accepted the frame and did nothing: a `com.superlogical.terminal.write` to a terminal in the same session wrote nothing. No log line. See "Plugin event emission". | 1203, lab |
| `rex block stream` | CLI help (`rex block stream BLOCK [DATA_REF]`) | Fails: `command "rex block stream" is not implemented`. Re-checked on 1203 on 2026-10-17 (lab). Use `data.connect` and the data channel instead. | 0.0.1792088193 and 1203, lab |
| Lua multiple assignment (`a, b = b, a`) | Lua 5.1 language (gopher-lua in `rex do`) | Both variables got the old value of `b`. Use a temporary local. See "Lua swap assignment is wrong". | 1203, lab |
| `rex.action` in a `rex do` script, called from another client | Docs (`rex.action` works in scripts) | `rex -C <client> do <name>` failed: "no action named". Declaring the action with `client.keymap` works. See "Rex Lua runtime". | 0.0.1792088193, lab |
| Required `args` of a config action, on `client.action` | Docs (`args`, a trailing `?` marks optional) | A palette-style `client.action` without a required argument ran with `args = {}`. Only the CLI checks arguments, and it refuses only unknown names. See "Command palette entries and forms" (branch `re/caller-identity`, commit `cd4bfa8`). | 1203, lab |
| `origin` value `cli` | Docs (`ctx.origin`: `key`, `palette`, `cli`, `api`, `script`) | A config action run with `rex do <action>` saw `ctx.origin = "api"`, not `cli`. A direct client action (`rex -C <client> do`) did get `origin: "cli"`. | 1203, lab |
| Reconnect token of a client that is not attached to a session | Hello `reconnect_token`; server log "control connection lost, keeping client for reconnect" | After `kill -9`, the client disappeared at once from `client.list` and `target.resolve`, and its token gave a new client ID. It works only for a client attached to a session. See "Lost clients and reconnect tokens". | 1203, lab |
| Kitty graphics Unicode placeholders (`U=1`) in the app | Server accepted the placement (`OK`) | The macOS app showed unknown-glyph boxes, not the image. The cause is unknown. PNG (`f=100`) uploads failed: `EINVAL: unsupported format`. See "Checkbox graphics probes". | 1203, lab server + real app |
| Plugin block as a terminal (wrap, or serve the data protocol) | Question only: no Rex feature claims it | `block.create` returning a terminal's ID does not wrap it. A plugin block has no data provider. See "Custom block types in the app". | 1203, lab |

## App update vs server (observed 2026-10-16, real machine, read-only checks)

- The Rex app updated from build 1173 to 1203 (Rex 0.0.1792088193-g7a93ec to 0.0.1792158932-g88fd41). The app relaunched (new PID). The server kept running: same PID 3514, old build, from a path the update deleted. The app then shows "Rex Server Restart Required". The app's text says: "Restarting the server may fix it, but you will lose your active sessions."
- So the client updates separately from the server, and terminals live in the server. Every pane is a direct child of the server process (server -> `login` -> shell). There is no separate PTY host. The decision "The Rex server is the core, and terminals die with it" still holds.
- Build 1203 moved the server bundle from `Helpers/Rex Server.app` to `Helpers/Rex Server.rex`. Panes started before the update keep a PATH entry for the deleted folder, so `rex` is not found in them until they get a new environment. The new CLI works against the old server.
- Build 1203 Lua (lab): `rex.json.encode/decode`, `rex.exec`, `rex.exec.start`, `rex.after`, `rex.every` exist. `io.popen` raises "io.popen is not available in Rex: use rex.exec". An empty table still encodes as `{}`.

## After the server restart on build 1203 (observed 2026-10-16, real machine, read-only)

- The 1203 server runs from a versioned folder outside the app bundle: `~/Library/Application Support/com.superlogical.rex/RexServer/Versions/1203/Rex Server.app/Contents/MacOS/rex`. Panes get that folder on PATH. So a later app update should not delete the binary of the running server (untested until the next update).
- New API method in 1203: `com.superlogical.terminal.tty_mode` returns the terminal's `echo`, `canonical` and `signals` flags. No other method was added or removed (reference diff).
- Codex CLI 0.163.0 reports OSC 7501 itself (`codex_tui::terminal_program_status` in the binary). One real turn: root record `working`, then `idle`, `app: codex`, `owner` the Codex process, `reason: report`, with no clear between them. Queued turns are untested.

## Driving the Rex app (observed 2026-10-16, Rex 1203, real machine, on the maintainer's request)

Check first: the Rex agent guide (`rex __internal skill`, section "Control Rex apps") documents app control (`rex client ls`, `rex -C <app> actions`, `rex -C <app> do session.select ...`). The API reference documents `detach_block` and `move_block`. This section records only what those sources do not say.

- **Remote Control.** The app refuses client actions by default: `403 refused`, "Rex.app is not accepting actions. Turn on Remote Control in its Settings under Rex Server." the maintainer turned it on. With it on, any process that can reach the server can call every app action, including `session.close` and `pane.close`.
- **Server focus does not move the app.** `session.focus_block` changed the session's active window on the server. The app kept its own tab. `session.select {session_id, window_id}` then showed the tab, with the server's `focused_block_id` as the app's focused pane (observed once).
- **Restore a detached pane to its own tab:** `move_block` beside any visible pane, `focus_block` on it, the app's `session.select` for that tab, then the app's `pane.move_to_new_tab`. Verified with a running Claude Code pane; its process survived detach and restore.
- **Layout facts for the UI framework (lab, Rex 1203, 2026-10-16):**
  - A block in a `session.new_window` layout takes `label` (`layout.block.label`). The label was set on the new block.
  - One control connection can attach to several sessions with repeated `session.attach`. It gets events from all of them, under one client ID.
  - Pane environment: the shell block's spawn options (`block_creator.list`, lab, 1203) are `args`, `command`, `cwd`, `exit`, `initial_input`, `login`, `path_prepend`, `shell`, `size`, `theme`. There is no `env`. A pane command runs with the login shell's environment (docs, "Shell scripting"). To set a variable, start the pane with `/usr/bin/env KEY=VALUE program`. Lua `rex.exec` takes `env`, but it runs a subprocess, not a pane.
  - Layout methods need the connection to be attached to the session: `session.new_window` on an unattached session failed with `409 conflict`, "stream is not attached to session".
- **The hub has one request queue.** Rex delivers `client.perform` requests to a client one at a time, and at most 16 can wait (reference). Agents, plugins and renderers all share the hub's queue. So every hub handler must be fast and must never wait for a client that can call the hub.
- **A timed-out request frees the queue.** Observed on Rex 0.0.1792158932 (lab, 2026-10-19, `tests/harness/tests/window_hold.lua`): the hub held one `client.perform` and did not reply. The next request to the hub came 10 s later, when the caller's `timeout_ms` (10000) ended the held request. The late reply of the held request had no receiver.
- **No input activity signal.** Neither the API reference nor the documented events (`reference/events.md`) have an input event or a last-input time. So the hub cannot tell whether the user is typing in an agent pane. Observed: a notice written while the maintainer typed submitted their unfinished text. Request #8 in `roadmap.md`.

## Checkbox graphics probes (Rex 1203, 2026-10-16)

- The maintainer confirmed that Ghostty is the only target. Do not assume that the UI must use portable text characters only.
- The isolated server accepted Kitty raw RGBA (`f=32`) uploads. Query, ordinary placement, and virtual placement (`U=1`) each returned `OK`.
- The same probe's PNG (`f=100`) uploads returned `EINVAL: unsupported format`. This result applies to the tested Rex build.
- `CSI 16 t` reported a cell height of 16 pixels and a cell width of 8 pixels in the lab.
- A Bubble Tea comparison used the installed `x/ansi/kitty` encoder and `tea.Raw` to upload six images. Unicode placeholders selected the image states. Mouse release and Tab + Space toggled the controls in the lab.
- The comparison must preserve exact RGB foreground values because those values contain image IDs. Automatic color detection removed colors in the first lab run. `tea.WithColorProfile(colorprofile.TrueColor)` preserved the IDs.
- A fresh terminal attachment preserved the placeholder characters and their diacritics. Image persistence through snapshots and app rendering remain unverified. Text capture alone does not prove either behavior.
- In the macOS app, Unicode-placeholder (virtual, `U=1`) placements did not show the image. The cause is unknown: no placeholder support, or a lost upload. The maintainer's screenshot showed U+10EEEE as unknown-glyph boxes and the diacritics as accents, with no image. The server had accepted the placement with `OK`. Direct placement (`a=T` with `c`/`r`) in the app was not checked. The maintainer chose the text checkbox and stopped the Kitty work (2026-10-16).
- The earlier text-only candidate used SGR 53 and SGR 4 for the top and bottom edges. The server preserved both codes. The maintainer's app screenshot showed no top edge and side strokes below the underline. Do not use that candidate.

### Lua coroutine context across callbacks

Observed on Rex `0.0.1792158932-g88fd41` (Rex 1203), 2026-10-16.
The probe is `spikes/lab/coroutine-context.lua`.
A timer creates a Lua coroutine and resumes it until `coroutine.yield()`.
A later timer resumes the same coroutine. The second resume returns `false` and `context canceled`.
The first callback's context does not remain valid for a later callback.
Do not use a coroutine to continue a Lua task across Rex callbacks on this build.
Use explicit task phases instead.

The [Lua API](https://www.superlogical.com/rex/docs/reference/lua.md) documents `rex.exec.start` and `detach: true`.
It also documents `false` environment values as removal of an inherited variable.
A detached read-only command can outlive the callback that starts it.
A lab probe started `/bin/sh -c 'printf done'` at script startup and read its successful result in a later timer.
Use a positive bounded timeout for `handle:wait` inside a callback.
The worktree fixture stalled with `wait(0)` after `running()` returned false.
The same fixture completed with `wait(0.001)` and a retry when the result was not ready.
The precise cause of that stall is unverified.

A separate lab probe used `rex.exec.start({'/bin/sleep', '5'}, {detach = true, timeout = 0.1})`.
A timer read the result after 0.5 seconds. Rex returned `timed_out: true`, `signal: "SIGTERM"`, and `code: -1`.
The command timeout therefore applies to detached commands on this build.

## Themes, color scheme and event scope (observed on Rex 1203, lab, 2026-10-16)

- `set_theme` applies only the owner's theme (or any client's while the block has no owner). A CLI `set_theme` on a block owned by another client (for example the hub's view panes) changes nothing visible. To test, take the block first with `session.own_block {mode: "take"}` on the same connection.
- `com.superlogical.terminal.resize` is the same: the client that created a block owns it, and a resize from another client is only remembered (`desired`). A lab test can take the hub's form pane and resize it to 90x80, so one `rex capture` shows the whole form (2026-10-17, `tests/profiles-lab.mjs`).
- Color scheme reports (DEC mode 2031): a pane that sends `CSI ? 2031 h` gets `CSI ? 997 ; 1 n` (dark) or `CSI ? 997 ; 2 n` (light) when a `set_theme` changes the `scheme`. No report is sent when the scheme does not change. `CSI ? 996 n` is answered also before any client has themed the pane (default: dark).
- A LazyRex renderer without 2031 handling and without a timer also switched light/dark within 1 s of each owner `set_theme`. So Rex sends some other trigger as well. Which one is not verified.
- `rex do <script> --all` delivers events from every session. Rex refuses `--all` together with `-s`. A plugin that needs both gets its session as a script argument (the hub passes `plugin_session=<id>`). Verified by Codex: foreign `terminal.pwd_changed` and `child_exited` arrive.
- Rex has no file-change events, and `rex.exec.start` has no exit callback (docs: `wait`, `running`, output callbacks). A plugin must poll `h:running()` while a command runs.
- `session.move_block` and `move_window` take one `session_id`: a block cannot move to another session.
- Floating layers: `session.new_layer` creates a floating layer in an existing window, in front of the tab's panes. No app-level modal or picker exists in the docs.

### Lua events across sessions

Observed on Rex `0.0.1792158932-g88fd41` (Rex 1203), 2026-10-16.
`rex do script.lua --all` delivers `terminal.pwd_changed` and `terminal.child_exited` from another session.
Rex refuses `--all` with `-s`: `use --all or --session, not both`.
A plugin can receive its control session as a Lua argument and attach that session explicitly.
The worktree test uses this arrangement. It refreshes the view after a foreign directory event and a foreign terminal exit.
Calling `session.attach_all` during script initialization did not deliver the foreign directory event in the initial test.
The cause of that difference is unverified. Use the documented `--all` option for server-wide Lua handlers.

### Terminal titles (Rex 1203, 2026-10-17)

- Observed on the real server: block method `com.superlogical.terminal.title` returns `{title, owner: {pid, name}}`. Claude Code sets the title of its pane, with an animated status glyph in front (for example "✳ ").
- Observed in the lab: a title change (OSC 2) reaches a session-attached control client as `block_event` with name `title_changed`. The hub renames an agent in a tab that has only Rex's generated label ("Window " + 8 characters of the window ID) after its terminal title, 1 s after the first change of a burst (`tests/harness/tests/pane_names.lua`).

### A pane's working directory (Rex 0.0.1792158932-g88fd41, 2026-10-17)

- Observed in the lab: a `com.superlogical.terminal.shell` pane with a plain login zsh answers `com.superlogical.terminal.pwd` with `{}`. The shell sends no OSC 7. Two OSC 7 sequences sent with `printf` from that shell (BEL and ST terminators, with `localhost` and with the host name) also left the answer `{}`. The cause is unknown.
- Observed in the lab: `com.superlogical.terminal.process` on the same pane answers `foreground {name: "zsh", cwd: <the shell's directory>}`. `child` is `/usr/bin/login` with no `cwd`.
- So a plugin that needs the source pane's directory reads `pwd` first, then `foreground.cwd`, then `child.cwd` (plugins/worktrees.lua, `repo_default`). The foreground can be an agent (Claude, Codex): its cwd is its project, which is the right default too (inferred).

### Lua helper loading and directory lists (Rex 1203, 2026-10-17)

Observed on Rex `0.0.1792158932-g88fd41` in `/tmp/rexlab-profiles-codex`:

- `loadfile`, `dofile`, and `require` are functions. `loadfile` with an absolute path loads a Lua file that returns a module table.
- In a script started with `rex do /absolute/script.lua`, `debug.getinfo(1, 'S').source` is the absolute script path, without a leading `@`. A loader can strip `@` if present, then require an absolute path. Do not resolve helpers against the process directory.
- `terminal.list_dir` preserves spaces, Unicode, newlines, and directory symlinks. With `files: false`, it omits ordinary files. The returned `path` is the base for entry names.
- An omitted `limit` returned all 1,100 entries in the test directory. With `limit: 200`, the result had 200 entries and `truncated: true`. A narrower `prefix` returned the ten matching names with `truncated: false`. The API has no cursor or offset.
- A Lua caller attached to another session can attach the source session and read its `terminal.pwd` and `terminal.list_dir`.

Repeatable test: `spikes/03-ui-framework/tests/profiles-directories-lab.mjs`. The helper-loading source-path probe is separate from that test.

### Listening ports in pane descendants

Observed on Rex `0.0.1792158932-g88fd41`, macOS, isolated lab, 2026-10-17.
`tests/tunnels-ports-lab.mjs` under spike 3 starts a Node parent in each of two
panes. Each parent starts a TCP listener child. The terminal `process.child.pid`,
`ps -axo pid=,ppid=,comm=`, and `lsof -nP -a -p <pids> -iTCP -sTCP:LISTEN -Fpcn`
identify both listeners and their owning panes without root. A separate listener
outside those pane trees is excluded. Detaching a pane keeps its listener in the
result with no window ID. Closing a pane removes it from the result.
This is a snapshot, not a guarantee against process exit or PID reuse.

### Tunnel blocks (observed 2026-10-17, Rex 0.0.1792158932-g88fd41, lab)

- In `rex do`, `debug.getinfo(1, 'S').source` is the absolute script path **without** the `@` prefix of standard Lua (`/tmp/rexlab-tunnels-claude/probe.lua`). Match it with `^@?(/.*)/[^/]*$`. The same is true for a chunk that the script loads with `loadfile` (observed on Rex 0.0.1792158932-g88fd41, lab, 2026-10-20: the harness runner; its failure location compared with `@<path>` and never matched).
- A plugin's `rex do` process inherits the lab server's environment. `os.getenv` reads variables that were set when `spikes/lab/server.sh` started. The hub config passes no plugin arguments, so the Tunnels lab uses this for its settings.
- A `session.new_block` shell block with `exit.on_completion = false` stays detached and callable after its command exits. `com.superlogical.terminal.process` then has `last_exit` and no `child`.
- `session.destroy` ended `opentunnel serve` in a detached block of that session: no opentunnel process remained, and the public URL failed with a TLS connect error within 3 seconds.
- `session.move_block` placed a detached block beside a pane of the same session (`direction: vertical`); `session.detach_block` detached it again, and the block kept running.
- `client.perform` of `hub.store.get` from a non-plugin client is refused by the hub (`only a registered plugin instance may call hub.store.get`). A lab test reads the `records` table of `hub.db` read-only instead.
- The `child.pid` of `com.superlogical.terminal.process` is the block command's own process. `ps -o etime=,command= -p <pid>` showed its full argv: `/bin/sh /tmp/rexlab-tunnels-claude/argv-probe.sh probe-key-123 x`, `etime` `00:03`. With `/bin/sh -c "sleep 20" arg0 key`, sh ran `sleep` in its place, and ps showed only `sleep 20`. So the Tunnels run script is a script file, and it keeps the shell as the parent of `serve` (no `exec`) to keep the op key visible.
- Rex reports a failed command as `last_exit.error` `"exit status <n>"` (observed for 1 and 3).

### App action "Go to Session" (Rex Beta 0.1.0 (1203), real app, 2026-10-18)

The app lists `session.select` ("Go to Session", args `session_id, window_id?`) among its client actions. Observed with the maintainer's approval: `rex -C <app client> do session.select --args '{"session_id":…,"window_id":…}'` returned no output and no error, and the app switched to that session and showed that tab (the maintainer confirmed). The hub uses it to switch the app to a template's new session.

### App action "Insert Path…" (Rex Beta 0.1.0 (1203), real app, 2026-10-18)

The Rex app lists `pane.insert_path` ("Insert Path…") and `pane.go_to_directory` ("Go to Directory…") among its client actions (`rex -C <app> actions`). Neither takes arguments, and the Rex docs do not describe them. Observed with the maintainer's approval, in a scratch pane:

- `rex -C <app client> do pane.insert_path` opens the app's directory browser on the user's screen, wherever the user is. It does not wait for or follow the server's focus: `session.focus_block` on another pane just before did not move the target.
- The chosen path is typed into the pane that has focus in the app, as input: absolute, with `~` for the home directory (the pane's directory was `/tmp`), shell-escaped (`Calibre\ Library/`), with a trailing `/` and no newline. Two picks typed two paths. Recorded bytes: `~/Projects/` then `~/Projects/LazyRex/docs/`.
- So a form that wants the native browser must have its own pane focused in the app (the user pressed a key in it), with the target field focused, and must unescape the text and expand `~`.
- Second probe (the maintainer ran it, same build): the probe pane enabled bracketed paste (mode 2004). A pick arrived as one bracketed paste: `ESC[200~~/Projects/ESC[201~`. Cancelling the browser with Esc sent no input to the pane. Typed keys arrived as plain bytes. So a renderer with bracketed paste on can take only a paste as a pick: typing after a cancelled browser is never mistaken for one.

### A perform error in Lua and a fake app client (observed 2026-10-19, Rex 0.0.1792158932-g88fd41, lab)

- In `rex do` Lua, `rex.call('client.perform', …)` returns no result and an error as text when the performer replies with `client.reply {error = {code, message}}`. Recorded: `stream response "control-159" failed: not_focused: refused by the mock (status=500 code=not_focused)`. The error is not a table, so `err.code` is nil. The code is the `code=` field at the end of the text. `plugins/templates.lua` reads it there. Inferred: the other plugins that test `type(err) == 'table'` lose the code in the same way.
- A control client that sends `hello` with `client = {kind = "app", name, label}` over the Unix socket shows `info.kind: "app"` in `client.list`. After `session.attach`, `session_ids` contains that session. It can declare `session.select` with `client.keymap` and gets `client.perform` requests for it. `tests/templates-lab.mjs` uses such a client as the fake app. It does not show that the real app switches: that check needs the real app.

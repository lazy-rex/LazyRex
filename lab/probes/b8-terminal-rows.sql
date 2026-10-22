-- B8 store probe rows for the release rehearsal (C3; codex-26, plan item D).
-- Only terminal ops and proved attempts: no plugin_hold row, no queued op, no open or unresolved
-- attempt. A rehearsal applies these rows to a copy of a hub.db that has the B8 tables, then starts
-- the older hub (it has no plugin_ops tables and must ignore them) and the B8 hub again (it must
-- read them unchanged). Active holds and unfinished attempts have their own lifecycle test.
-- The rehearsal applies this file to a lab copy only, never to a live store. TestB8ProbeRows checks it
-- against the current schema, and that the hold reads state none with nothing unresolved.
BEGIN;
INSERT INTO plugin_ops (op_id, scope, key, fingerprint, origin, kind, entry, name, expect, input, target, phase, rev,
  confirmed, base, output, entry_identity, result, error, created, updated)
VALUES
  ('probe-op-stop', 'agent:probe', 'probe-stop', 'probe', '{"kind":"agent","id":"probe"}', 'stop', 'managed:probe', 'probe',
   '', '', '', 'done', 1, '', '', '', '["managed:probe","/probe/src"]', 'stop: {"desired":"stopped"}', '', 1792600000000, 1792600000000),
  ('probe-op-settings', 'agent:probe', 'probe-settings', 'probe', '{"kind":"agent","id":"probe"}', 'settings', 'managed:probe', 'probe',
   '', '{"args":{"k":"v"}}', '', 'done', 1, '', '{"id":"probe","source":"/probe/src","args":{},"watch_roots":[]}',
   '0000000000000000000000000000000000000000000000000000000000000000', '', 'reload: {"errors":{}}', '', 1792600001000, 1792600001000);
INSERT INTO plugin_op_attempts (attempt, op_id, call, expect_run, at, outcome, bound_at, target_instance, target_block,
  target_session, intent_id, sent_at, proof, resolved_by, intended)
VALUES
  ('probe-attempt-stop', 'probe-op-stop', 'stop', '', 1792600000000, 'stop: {"desired":"stopped"}', '2026-10-22T00:00:00Z',
   'probe-1-aaaaaaaa', 'block:probe', 'session:probe', '', '2026-10-22T00:00:00Z', 'applied: the call returned', '', ''),
  ('probe-attempt-write', 'probe-op-settings', 'write:managed', '', 1792600001000, 'written', '2026-10-22T00:00:01Z',
   '', '', '', '', '2026-10-22T00:00:01Z', 'applied: the rename returned', '',
   '0000000000000000000000000000000000000000000000000000000000000000'),
  ('probe-attempt-reload', 'probe-op-settings', 'reload', '', 1792600001500, 'reload: {"errors":{}}', '',
   '', '', '', '', '', 'applied: the reload returned', '', '');
COMMIT;

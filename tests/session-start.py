"""Check the hook's input handling and CLI boundary without changing user configuration."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest


HOOK = Path(__file__).resolve().parents[1] / "hooks" / "session-start.py"


class SessionStartTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="lazyrex-hook-")
        self.root = Path(self.directory.name)
        self.capture = self.root / "args.json"
        self.rex = self.root / "rex with spaces"
        self.rex.write_text("#!/usr/bin/env python3\nimport json,os,sys\n"
                            "if sys.argv[-3:]==['client','ls','--json']:\n"
                            " print(json.dumps({'clients':json.loads(os.environ['HOOK_CLIENTS'])}));sys.exit(0)\n"
                            "json.dump(sys.argv[1:],open(os.environ['HOOK_CAPTURE'],'w'))\n"
                            "print('This output must not become model context.')\n")
        self.rex.chmod(0o755)
        self.env = dict(os.environ, REX_SERVER="unix:///tmp/hook-test/rex.sock",
                        REX_SESSION="session:lab", REX_BLOCK="block:lab",
                        REX_BIN=str(self.rex), HOOK_CAPTURE=str(self.capture),
                        HOOK_CLIENTS=json.dumps([{"client_id": "client:hub",
                          "info": {"label": "lazyrex-hub"}, "connection_state": "connected"}]))

    def tearDown(self):
        self.directory.cleanup()

    def run_hook(self, event, kind="codex"):
        result = subprocess.run([sys.executable, str(HOOK), kind],
                                input=json.dumps(event), text=True, capture_output=True,
                                env=self.env, timeout=5)
        self.assertEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")
        self.assertEqual(result.stderr, "")
        return json.loads(self.capture.read_text()) if self.capture.exists() else None

    def test_preserves_json_and_uses_only_the_explicit_server(self):
        conversation = 'quote " space 雪; $(touch never)'
        for kind in ("codex", "claude"):
            args = self.run_hook({"hook_event_name": "SessionStart",
                                  "session_id": conversation, "source": "clear"}, kind)
            self.assertEqual(args[:7], ["-S", self.env["REX_SERVER"], "--autostart=false",
                                       "--timeout", "2s", "-C", "client:hub"])
            self.assertEqual(args[7:10], ["do", "hub.identity.report", "--args"])
            self.assertEqual(json.loads(args[10]), {"kind": kind,
                             "conversation_id": conversation, "source": "clear", "block": "block:lab"})

    def test_claude_prompt_repairs_a_missing_start_report(self):
        event = {"hook_event_name": "UserPromptSubmit", "session_id": "new-id"}
        self.assertIsNone(self.run_hook(event, "codex"))
        args = self.run_hook(event, "claude")
        self.assertEqual(json.loads(args[10]), {"kind": "claude",
                         "conversation_id": "new-id", "source": "prompt", "block": "block:lab"})

    def test_discovery_ignores_lost_clients_and_refuses_multiple_hubs(self):
        event = {"hook_event_name": "SessionStart", "session_id": "id", "source": "startup"}
        clients = json.loads(self.env["HOOK_CLIENTS"])
        clients.append({"client_id": "client:old", "info": {"label": "lazyrex-hub"},
                        "connection_state": "lost"})
        self.env["HOOK_CLIENTS"] = json.dumps(clients)
        self.assertEqual(self.run_hook(event)[6], "client:hub")
        self.capture.unlink()
        clients[1]["connection_state"] = "connected"
        self.env["HOOK_CLIENTS"] = json.dumps(clients)
        self.assertIsNone(self.run_hook(event))

    def test_outside_rex_does_not_wait_for_input(self):
        del self.env["REX_BLOCK"]
        process = subprocess.Popen([sys.executable, str(HOOK), "codex"],
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, env=self.env)
        try:
            self.assertEqual(process.wait(timeout=1), 0)
            self.assertFalse(self.capture.exists())
        finally:
            process.communicate()

    def test_ignores_irrelevant_or_invalid_input(self):
        for event in (None, [], {}, {"hook_event_name": "Stop"},
                      {"hook_event_name": "SessionStart", "session_id": 9, "source": "startup"},
                      {"hook_event_name": "SessionStart", "session_id": "id", "source": "unknown"}):
            self.assertIsNone(self.run_hook(event))

    def test_missing_rex_is_advisory(self):
        self.env["REX_BIN"] = str(self.root / "missing")
        self.assertIsNone(self.run_hook({"hook_event_name": "SessionStart",
                                        "session_id": "id", "source": "startup"}))

    def test_no_hub_is_advisory(self):
        self.rex.write_text("#!/bin/sh\necho 'hub not found' >&2\nexit 4\n")
        self.assertIsNone(self.run_hook({"hook_event_name": "SessionStart",
                                        "session_id": "id", "source": "resume"}))

    def test_unresponsive_rex_has_a_deadline(self):
        self.rex.write_text("#!/usr/bin/env python3\nimport time\ntime.sleep(10)\n")
        started = time.monotonic()
        self.assertIsNone(self.run_hook({"hook_event_name": "SessionStart",
                                        "session_id": "id", "source": "compact"}))
        self.assertLess(time.monotonic() - started, 4)


if __name__ == "__main__":
    unittest.main()

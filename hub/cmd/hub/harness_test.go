package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// withHarness adds a test-only row to the table for this test.
func withHarness(t *testing.T, h *harness) {
	t.Helper()
	old := harnesses
	harnesses = append(append([]*harness(nil), old...), h)
	t.Cleanup(func() { harnesses = old })
}

func fakeHarness(promptReport bool, resumePrompt string) *harness {
	return &harness{
		kind: "fake", name: "Fake", product: "Fake CLI", exe: "fake-cli",
		promptReport: promptReport, reportsAt: "reports when the test says", trustHook: true,
		exitKeys: 3, exitGap: time.Millisecond,
		envUnset:  []string{"FAKE_SESSION"},
		configEnv: "FAKE_HOME", configDir: ".fake", hooksFile: "hooks.json",
		skill:        func(_, config string) string { return filepath.Join(config, "skill.md") },
		command:      func(p string) []string { return []string{p, "${task}"} },
		resume:       func(p string) []string { return []string{p, "--continue", "${conversation_id}"} },
		resumePrompt: resumePrompt,
	}
}

// The table keeps every text and value of the two CLIs as they were before it (lead-harness: no
// behaviour change).
func TestHarnessTableKeepsTheTexts(t *testing.T) {
	if got := harnessKinds(); got != "claude or codex" {
		t.Errorf("kinds: %q", got)
	}
	if got, want := harnessHelp(), "Claude Code reports at its start; Codex can report only at its first prompt. "+
		"If it stays, check the LazyRex hook (lazyrex-hub setup; Codex: trust it with /hooks)."; got != want {
		t.Errorf("help:\n%s\nwant\n%s", got, want)
	}
	want := []string{"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT",
		"CLAUDE_PID", "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN",
		"CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_TOOL_CALL_ID"}
	if got := agentEnvUnset(); !slices.Equal(got, want) {
		t.Errorf("env: %v", got)
	}
	lua := paletteLua(nil)
	for _, k := range [][2]string{{"codex", "Codex"}, {"claude", "Claude"}} {
		entry := `rex.action {
  name = "lazyrexhub_restart_` + k[0] + `",
  title = "LazyRex: Restart ` + k[1] + ` agents…",
  category = "LazyRex",
  description = "List the current ` + k[1] + ` agents, then stop and resume them in new panes",
  keywords = "agent restart ` + k[0] + `",
  run = lazyrex_restart_form("` + k[0] + `"),
}
`
		if !strings.Contains(lua, entry) {
			t.Errorf("no restart entry for %s:\n%s", k[0], entry)
		}
	}
	c, x := harnessOf("claude"), harnessOf("codex")
	if c.exitKeys != 2 || c.exitGap != 300*time.Millisecond || x.exitKeys != 1 || !c.promptReport || x.promptReport {
		t.Errorf("exit keys or prompt report: %+v %+v", c, x)
	}
	if c.resumePrompt != "" || x.resumePrompt != "LazyRex: this conversation was resumed in a new pane." {
		t.Errorf("resume prompts: %q %q", c.resumePrompt, x.resumePrompt)
	}
}

// The third fake adapter (lead-harness item 2): a test-only row makes the hook, the identity, the
// restart and the setup paths accept the kind, with no other hub edit. Both values of promptReport.
func TestHarnessFakeAdapter(t *testing.T) {
	for _, prompt := range []bool{true, false} {
		withHarness(t, fakeHarness(prompt, ""))

		// The hook.
		if src, conv, ok := hookEvent("fake", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "s1"}); !ok || src != "startup" || conv != "s1" {
			t.Errorf("prompt %v: SessionStart: %q %q %v", prompt, src, conv, ok)
		}
		if _, _, ok := hookEvent("fake", map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s1"}); ok != prompt {
			t.Errorf("prompt %v: UserPromptSubmit reported %v", prompt, ok)
		}
		if _, _, ok := hookEvent("nope", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "s1"}); ok {
			t.Error("an unknown kind reported")
		}

		// The identity report: the kind passes; the next check (the conversation ID) refuses.
		h := &Hub{}
		if _, err := h.identityReport(context.Background(), perform{}, map[string]any{"kind": "fake"}); err == nil || strings.Contains(err.Error(), "kind must") {
			t.Errorf("prompt %v: report of the fake kind: %v", prompt, err)
		}
		if _, err := h.identityReport(context.Background(), perform{}, map[string]any{"kind": "nope"}); err == nil || err.Error() != "invalid: kind must be claude, codex or fake" {
			t.Errorf("an unknown kind: %v", err)
		}

		// The restart: the CLI check, the form and its palette entry.
		rec := paneRecord{pid: 7, start: "1.000001", path: "/opt/fake-cli", invoked: "/opt/fake-cli"}
		if got := restartCheck(paneState{child: 7, path: "/opt/fake-cli", invoked: "/opt/fake-cli"}, rec, rec.start, "/opt/fake-cli", "fake"); got != "" {
			t.Errorf("restart check of the fake CLI: %q", got)
		}
		if got := restartCheck(paneState{child: 7, path: "/bin/sh", invoked: "/bin/sh"}, paneRecord{7, rec.start, "/bin/sh", "/bin/sh"}, rec.start, "/bin/sh", "fake"); !strings.Contains(got, "not the fake CLI") {
			t.Errorf("a shell in a fake pane: %q", got)
		}
		if _, err := h.adminRestartForm(context.Background(), perform{}, map[string]any{"kind": "fake"}); err == nil || !strings.Contains(err.Error(), "ctx.client_id") {
			t.Errorf("restart form of the fake kind: %v", err)
		}
		if !strings.Contains(paletteLua(nil), `run = lazyrex_restart_form("fake")`) {
			t.Error("no palette entry for the fake kind")
		}

		// Setup: the agent, its profile and its hook file.
		e, _ := setupTestEnv(t, "", true)
		e.getenv = func(k string) string { return map[string]string{"FAKE_HOME": "/f"}[k] }
		as := e.agents()
		fa := as[len(as)-1]
		if fa.kind != "fake" || fa.hooks != "/f/hooks.json" || fa.skill != "/f/skill.md" || filepath.Base(fa.program) != "fake-cli" {
			t.Errorf("setup agent: %+v", fa)
		}
		b, err := setupProfiles(e, as)
		if err != nil || !strings.Contains(string(b), `"id": "fake"`) || !strings.Contains(string(b), `"--continue"`) {
			t.Errorf("profiles: %v\n%s", err, b)
		}
		f := snapshotFile(filepath.Join(t.TempDir(), "hooks.json"), 0o644)
		hookFile(f, "fake", "hub hook fake")
		if strings.Contains(string(f.next), "UserPromptSubmit") != prompt || !strings.Contains(string(f.next), "SessionStart") {
			t.Errorf("prompt %v: hook file:\n%s", prompt, f.next)
		}

		// Help and the pane environment.
		if !strings.Contains(harnessHelp(), "Fake CLI reports when the test says") || !strings.Contains(harnessHelp(), "Fake: trust it with /hooks") {
			t.Errorf("help: %s", harnessHelp())
		}
		if !slices.Contains(spawnArgv(&spawnRow{}, Profile{Command: []string{"/bin/true"}}, nil), "FAKE_SESSION") {
			t.Error("a pane keeps FAKE_SESSION")
		}
		harnesses = harnesses[:len(harnesses)-1]
	}
}

// No hub code outside harness.go names a CLI in a string (lead-harness item 2): the source has no
// string literal with claude or codex. The one exception is an example ID in an action description,
// which testdata/actions pins byte for byte.
func TestHarnessNamesOnlyInTheTable(t *testing.T) {
	allowed := map[string]bool{"actions.go": true} // `a unique ID prefix ("claude:7a18")`
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "harness.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			low := strings.ToLower(lit.Value)
			if !strings.Contains(low, "claude") && !strings.Contains(low, "codex") {
				return true
			}
			if allowed[name] && strings.Contains(lit.Value, `\"claude:7a18\"`) && !strings.Contains(strings.ReplaceAll(low, "claude:7a18", ""), "claude") {
				return true
			}
			t.Errorf("%s: a string names a CLI: %.80s", fset.Position(lit.Pos()), lit.Value)
			return true
		})
	}
}

// The resume consumer of the table (lead-harness2 item 1): a fake row with a resumePrompt gets that
// text once in the new pane (prompt_sending, then prompt_sent); a row without one gets no write, waits
// in launched, and is unknown after resumeRebindWait. Both rebind from the target's pane.
func TestHarnessFakeResumePrompt(t *testing.T) {
	for _, text := range []string{"Fake: resumed.", ""} {
		withHarness(t, fakeHarness(false, text))
		h := resumeTestHub(t)
		h.ui = &uiState{kick: make(chan struct{}, 1), sess: map[string]string{"session:test": uiSessionKey}} // the work session: no attach
		ctx := context.Background()
		var writes []string
		pid := os.Getpid() // a real process: the hub compares its start time between looks
		h.rex = payloadRex(t, func(method string, payload json.RawMessage) (any, string) {
			switch method {
			case "com.superlogical.terminal.process":
				return map[string]any{"child": map[string]any{"pid": pid}, "foreground": map[string]any{"pid": pid}}, ""
			case "com.superlogical.terminal.program_status":
				return map[string]any{"records": []any{map[string]any{"id": "", "state": "idle", "owner": map[string]any{"pid": pid}}}}, ""
			case "com.superlogical.terminal.write":
				writes = append(writes, string(payload))
			}
			return map[string]any{}, ""
		})
		mustExec(t, h, `INSERT INTO identities (identity, kind, conversation_id, state, block_id, predecessor, name, created, updated) VALUES ('fake:a', 'fake', 'a', 'detached', 'block:old', '', '', 't0', 't0')`)
		if _, err := h.adminResume(ctx, map[string]any{"key": "k", "agent": "fake:a"}); err != nil {
			t.Fatal(err)
		}
		// The launch itself is covered elsewhere: the new pane is open.
		mustExec(t, h, `INSERT INTO spawns (spawn_id,caller,key,request,profile,title,agent_id,parent,status,created,session_id,block_id)
 VALUES ('spawn-new','user','key-new','{}','agent','Agent 1','spawn:spawn-test','user','open','2026-10-16T23:00:00Z','session:test','block:new')`)
		mustExec(t, h, `UPDATE agent_op_targets SET phase = 'launched', spawn_id = 'spawn-new', block_id = 'block:new', updated = ? WHERE identity = 'fake:a'`,
			time.Now().UTC().Format(time.RFC3339Nano))
		phase := func() (p, d string) {
			h.db.QueryRow(`SELECT phase, detail FROM agent_op_targets WHERE identity = 'fake:a'`).Scan(&p, &d)
			return p, d
		}
		for range 2 {
			if _, err := h.resumeStep(ctx); err != nil {
				t.Fatal(err)
			}
		}
		p, d := phase()
		if text != "" {
			if p != "prompt_sent" || len(writes) != 2 || !strings.Contains(writes[0], text) || !strings.Contains(writes[1], `\r`) {
				t.Fatalf("prompt %q: phase %s %q, writes %q", text, p, d, writes)
			}
		} else {
			if p != "launched" || len(writes) != 0 {
				t.Fatalf("no prompt: phase %s %q, writes %q", p, d, writes)
			}
			mustExec(t, h, `UPDATE agent_op_targets SET updated = ? WHERE identity = 'fake:a'`,
				time.Now().Add(-resumeRebindWait-time.Second).UTC().Format(time.RFC3339Nano))
			h.resumeStep(ctx)
			if p, d := phase(); p != "unknown" || !strings.Contains(d, "no hook report") || len(writes) != 0 {
				t.Fatalf("no prompt, after the wait: phase %s %q, writes %q", p, d, writes)
			}
		}
		// The hook reports from the new pane: the target is rebound, and nothing more is written.
		mustExec(t, h, `UPDATE identities SET state = 'current', block_id = 'block:new' WHERE identity = 'fake:a'`)
		n := len(writes)
		h.resumeStep(ctx)
		if p, _ := phase(); p != "rebound" || len(writes) != n {
			t.Fatalf("prompt %q: after the report: phase %s, writes %d -> %d", text, p, n, len(writes))
		}
		harnesses = harnesses[:len(harnesses)-1]
	}
}

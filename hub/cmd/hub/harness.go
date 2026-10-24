package main

import (
	"path/filepath"
	"strings"
	"time"
)

// harness is one agent CLI that the hub knows (lead-harness, boundary decisions 2026-10-23). Every
// fact of the hub that depends on the CLI is a field here; other hub code reads only this table, so
// a new CLI is one row.
type harness struct {
	kind    string // the identity kind, the argument of `lazyrex-hub hook`, and Rex's `skill setup --agent` name
	name    string // the short name: the setup profile and the restart form title
	product string // the name in setup output and help text
	exe     string // the base name of the CLI executable; a restart refuses a pane that runs another
	// promptReport: the hook reports at UserPromptSubmit too (setup installs that event), not only
	// at SessionStart.
	promptReport bool
	reportsAt    string // when the CLI reports, for the help text
	trustHook    bool   // the CLI asks the user to trust a new hook with /hooks
	// exitKeys Ctrl-C end the CLI, exitGap apart. Claude: two, 300 ms (S1, 2026-10-22: 300 ms exits,
	// 1.5 s does not). Codex: one.
	exitKeys int
	exitGap  time.Duration
	// envUnset are conversation, nesting and per-process variables of this CLI that an agent pane
	// must not inherit from another agent (a pane inherits the Rex server's environment; a server
	// started from an agent's shell carries that agent's variables, observed in the lab on Rex 1203).
	// With CLAUDE_CODE_CHILD_SESSION set, Claude ignores --session-id (lab, 2.1.302). Deliberate
	// configuration (for example CLAUDE_EFFORT, CODEX_HOME) stays.
	envUnset []string
	// The configuration directory is $configEnv, else ~/configDir. hooksFile is the hook file in it.
	configEnv, configDir, hooksFile string
	skill                           func(home, config string) string // the skill file
	command, resume                 func(program string) []string    // the setup profile's templates
	// resumePrompt is the first prompt of a resumed agent, "" for none. A resumed Codex runs its hook
	// (and so rebinds) only at its first prompt (S1, 2026-10-22); Claude reports at its start. The
	// text names no plugin: a plugin wakes the agent for its own reasons (lead-harness2).
	resumePrompt string
}

// harnesses is the table, in the order of setup and the palette.
var harnesses = []*harness{
	{
		kind: "claude", name: "Claude", product: "Claude Code", exe: "claude",
		promptReport: true, reportsAt: "reports at its start",
		exitKeys: 2, exitGap: 300 * time.Millisecond,
		envUnset: []string{"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT",
			"CLAUDE_PID", "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN"},
		configEnv: "CLAUDE_CONFIG_DIR", configDir: ".claude", hooksFile: "settings.json",
		skill:   func(_, config string) string { return filepath.Join(config, "skills", "lazyrex", "SKILL.md") },
		command: func(p string) []string { return []string{p, "${task}"} },
		resume:  func(p string) []string { return []string{p, "--resume", "${conversation_id}"} },
	},
	{
		kind: "codex", name: "Codex", product: "Codex", exe: "codex",
		reportsAt: "can report only at its first prompt", trustHook: true,
		exitKeys:  1,
		envUnset:  []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_TOOL_CALL_ID"},
		configEnv: "CODEX_HOME", configDir: ".codex", hooksFile: "hooks.json",
		// Codex reads user skills from ~/.agents/skills; $CODEX_HOME/skills is its deprecated
		// location (codex-rs ext/skills host_roots.rs).
		skill: func(home, _ string) string { return filepath.Join(home, ".agents", "skills", "lazyrex", "SKILL.md") },
		// Without --no-daemon, Codex starts MCP servers outside the pane (docs/hub.md).
		command:      func(p string) []string { return []string{p, "--no-daemon", "${task}"} },
		resume:       func(p string) []string { return []string{p, "--no-daemon", "resume", "${conversation_id}"} },
		resumePrompt: "LazyRex: this conversation was resumed in a new pane.",
	},
}

// harnessOf is the row of kind, or nil.
func harnessOf(kind string) *harness {
	for _, h := range harnesses {
		if h.kind == kind {
			return h
		}
	}
	return nil
}

// harnessName is the short name of kind, "" for an unknown kind.
func harnessName(kind string) string {
	if h := harnessOf(kind); h != nil {
		return h.name
	}
	return ""
}

// harnessKinds is the kinds for an error message: "claude or codex".
func harnessKinds() string {
	ks := make([]string, len(harnesses))
	for i, h := range harnesses {
		ks[i] = h.kind
	}
	if len(ks) < 2 {
		return strings.Join(ks, "")
	}
	return strings.Join(ks[:len(ks)-1], ", ") + " or " + ks[len(ks)-1]
}

// agentEnvUnset is every row's envUnset, in table order: every pane drops them, also an accessory
// (the user can start an agent in it).
func agentEnvUnset() []string {
	var out []string
	for _, h := range harnesses {
		out = append(out, h.envUnset...)
	}
	return out
}

// harnessHelp is the help line of an agent that has not reported: when each CLI reports, and the
// hook check. "Claude Code reports at its start; Codex can report only at its first prompt. If it
// stays, check the LazyRex hook (lazyrex-hub setup; Codex: trust it with /hooks)."
func harnessHelp() string {
	var when, trust []string
	for _, h := range harnesses {
		when = append(when, h.product+" "+h.reportsAt)
		if h.trustHook {
			trust = append(trust, "; "+h.name+": trust it with /hooks")
		}
	}
	return strings.Join(when, "; ") + ". If it stays, check the LazyRex hook (lazyrex-hub setup" + strings.Join(trust, "") + ")."
}

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// skillStatusJSON is a `rex skill status --json` report: the skill and the hook of each agent in
// state.
func skillStatusJSON(state string, agents ...string) []byte {
	type item struct {
		Kind  string `json:"kind"`
		State string `json:"state"`
	}
	type harness struct {
		ID    string `json:"id"`
		Items []item `json:"items"`
	}
	var hs []harness
	for _, a := range agents {
		hs = append(hs, harness{a, []item{{"skill", state}, {"hook", state}}})
	}
	b, _ := json.Marshal(map[string]any{"scope": "user", "harnesses": hs})
	return b
}

func TestRexSkillCurrent(t *testing.T) {
	both := []string{"claude", "codex"}
	for _, c := range []struct {
		name  string
		b     []byte
		err   error
		kinds []string
		want  bool
	}{
		{"both current", skillStatusJSON("current", "claude", "codex"), nil, both, true},
		{"one agent missing", skillStatusJSON("current", "claude"), nil, both, false},
		{"outdated", skillStatusJSON("outdated", "claude", "codex"), nil, both, false},
		{"foreign (a skill that Rex did not install)", skillStatusJSON("foreign", "claude"), nil, []string{"claude"}, false},
		{"unreadable", skillStatusJSON("unreadable", "claude"), nil, []string{"claude"}, false},
		{"only the skill", []byte(`{"harnesses":[{"id":"claude","items":[{"kind":"skill","state":"current"},{"kind":"hook","state":"missing"}]}]}`), nil, []string{"claude"}, false},
		{"only the hook", []byte(`{"harnesses":[{"id":"claude","items":[{"kind":"hook","state":"current"}]}]}`), nil, []string{"claude"}, false},
		{"malformed", []byte(`{"harnesses":`), nil, both, false},
		{"empty", nil, nil, both, false},
		{"the status failed", skillStatusJSON("current", "claude", "codex"), errors.New("exit status 1"), both, false},
		{"no agents", skillStatusJSON("current", "claude"), nil, nil, false},
	} {
		if got := rexSkillCurrent(c.b, c.err, c.kinds); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// skillRun is one setup run with the files all answered yes and the plugins installed, so that the
// Rex skill question comes last; answer is its answer ("" = end of input).
type skillRun struct {
	out      string
	code     int
	setups   [][]string // the args of each rex skill setup call
	statuses int        // the rex skill status calls
}

func runSkillSetup(t *testing.T, answer string, status func(n int) ([]byte, error), setup error, edit func(*setupEnv)) skillRun {
	t.Helper()
	e, out := setupTestEnv(t, strings.Repeat("y\n", 8)+answer, true)
	e.installed = func(string) bool { return true }
	var r skillRun
	e.skillStatus = func() ([]byte, error) { r.statuses++; return status(r.statuses) }
	e.skillSetup = func(args []string, w io.Writer) error {
		r.setups = append(r.setups, args)
		io.WriteString(w, "rex skill setup output\n")
		return setup
	}
	if edit != nil {
		edit(&e)
	}
	r.code = runSetup(e, filepath.Join(e.home, ".lazyrex"), false)
	r.out = out.String()
	return r
}

// Setup offers `rex skill setup` for exactly the agents that it found, and claims "installed" only
// when rex skill status says the skill and the hook of each are current (codex-24's plan review).
func TestSetupSkillOffer(t *testing.T) {
	missing := func(int) ([]byte, error) { return skillStatusJSON("missing", "claude", "codex"), nil }
	afterSetup := func(n int) ([]byte, error) { // missing before the setup, current after it
		if n == 1 {
			return missing(n)
		}
		return skillStatusJSON("current", "claude", "codex"), nil
	}
	both := []string{"skill", "setup", "--agent", "claude", "--agent", "codex"}
	prompt := "Install Rex's own skill and hooks for claude, codex now (rex skill setup --agent claude --agent codex)? [y/N] "
	printed := "Install Rex's own skill and hooks: rex skill setup --agent claude --agent codex\n"
	installed := "Rex's skill and hooks: installed for claude, codex."

	t.Run("already current: no question", func(t *testing.T) {
		r := runSkillSetup(t, "y\n", func(int) ([]byte, error) { return skillStatusJSON("current", "claude", "codex"), nil }, nil, nil)
		if r.code != 0 || r.setups != nil || strings.Contains(r.out, prompt) || !strings.Contains(r.out, installed) || strings.Contains(r.out, printed) {
			t.Fatalf("exit %d, setups %v:\n%s", r.code, r.setups, r.out)
		}
	})
	t.Run("yes: setup, then status confirms", func(t *testing.T) {
		r := runSkillSetup(t, "y\n", afterSetup, nil, nil)
		if r.code != 0 || !reflect.DeepEqual(r.setups, [][]string{both}) || r.statuses != 2 ||
			!strings.Contains(r.out, prompt) || !strings.Contains(r.out, "rex skill setup output\n"+installed) || strings.Contains(r.out, printed) {
			t.Fatalf("exit %d, setups %v, statuses %d:\n%s", r.code, r.setups, r.statuses, r.out)
		}
	})
	for _, answer := range []string{"n\n", "\n", "no\n", ""} {
		t.Run("declined or no answer "+strings.TrimSpace(answer), func(t *testing.T) {
			r := runSkillSetup(t, answer, missing, nil, nil)
			if r.code != 0 || r.setups != nil || !strings.Contains(r.out, printed) || strings.Contains(r.out, installed) {
				t.Fatalf("exit %d, setups %v:\n%s", r.code, r.setups, r.out)
			}
		})
	}
	t.Run("an earlier end of input: no question", func(t *testing.T) {
		r := runSkillSetup(t, "", missing, nil, func(e *setupEnv) { *e = withAnswers(*e, "y\ny\n") })
		if r.setups != nil || strings.Contains(r.out, prompt) || !strings.Contains(r.out, printed) {
			t.Fatalf("exit %d, setups %v:\n%s", r.code, r.setups, r.out)
		}
	})
	t.Run("setup fails (a Rex conflict)", func(t *testing.T) {
		r := runSkillSetup(t, "y\n", afterSetup, errors.New("exit status 1"), nil)
		if r.code != 1 || !strings.Contains(r.out, "Rex skill setup failed: exit status 1") || strings.Contains(r.out, installed) || !strings.Contains(r.out, printed) {
			t.Fatalf("exit %d:\n%s", r.code, r.out)
		}
	})
	for name, after := range map[string]func(int) ([]byte, error){
		"still missing": missing,
		"malformed":     func(n int) ([]byte, error) { return []byte("{"), nil },
		"failed":        func(n int) ([]byte, error) { return nil, errors.New("exit status 1") },
		"foreign":       func(n int) ([]byte, error) { return skillStatusJSON("foreign", "claude", "codex"), nil },
	} {
		t.Run("setup succeeds, status "+name, func(t *testing.T) {
			status := after
			if name != "still missing" {
				status = func(n int) ([]byte, error) {
					if n == 1 {
						return missing(n)
					}
					return after(n)
				}
			}
			r := runSkillSetup(t, "y\n", status, nil, nil)
			if r.code != 1 || !strings.Contains(r.out, "does not report the skill and hooks as current for claude, codex") || strings.Contains(r.out, installed) || !strings.Contains(r.out, printed) {
				t.Fatalf("exit %d:\n%s", r.code, r.out)
			}
		})
	}
	t.Run("a failed first status is no claim: the question comes", func(t *testing.T) {
		r := runSkillSetup(t, "n\n", func(int) ([]byte, error) { return nil, errors.New("rex: not found") }, nil, nil)
		if r.code != 0 || !strings.Contains(r.out, prompt) || strings.Contains(r.out, installed) {
			t.Fatalf("exit %d:\n%s", r.code, r.out)
		}
	})
	t.Run("only the agents that setup found", func(t *testing.T) {
		r := runSkillSetup(t, "y\n", func(n int) ([]byte, error) {
			if n == 1 {
				return skillStatusJSON("missing", "claude"), nil
			}
			return skillStatusJSON("current", "claude"), nil
		}, nil, func(e *setupEnv) {
			tools := filepath.Join(e.home, "tools")
			e.lookPath = func(n string) (string, error) {
				if n == "codex" {
					return "", errors.New("not found")
				}
				return filepath.Join(tools, n), nil
			}
			*e = withAnswers(*e, strings.Repeat("y\n", 6)+"y\n")
		})
		want := [][]string{{"skill", "setup", "--agent", "claude"}}
		if r.code != 0 || !reflect.DeepEqual(r.setups, want) || !strings.Contains(r.out, "Rex's skill and hooks: installed for claude.") {
			t.Fatalf("exit %d, setups %v:\n%s", r.code, r.setups, r.out)
		}
	})
	t.Run("no agent found: no question, no command", func(t *testing.T) {
		r := runSkillSetup(t, "y\n", missing, nil, func(e *setupEnv) {
			e.lookPath = func(string) (string, error) { return "", errors.New("not found") }
			*e = withAnswers(*e, strings.Repeat("y\n", 4)+"y\n")
		})
		if r.code != 0 || r.setups != nil || r.statuses != 0 || strings.Contains(r.out, "rex skill setup") {
			t.Fatalf("exit %d, setups %v, statuses %d:\n%s", r.code, r.setups, r.statuses, r.out)
		}
	})
}

// -print and a run without a terminal ask nothing and run no rex skill command; they print the
// command with the agents that setup found.
func TestSetupSkillPrintOnly(t *testing.T) {
	for _, c := range []struct{ print, tty bool }{{true, true}, {false, false}} {
		e, out := setupTestEnv(t, "y\ny\n", c.tty)
		calls := 0
		e.skillStatus = func() ([]byte, error) { calls++; return nil, errors.New("called") }
		e.skillSetup = func([]string, io.Writer) error { calls++; return errors.New("called") }
		runSetup(e, filepath.Join(e.home, ".lazyrex"), c.print)
		if calls != 0 || !strings.Contains(out.String(), "setup offers `rex skill setup --agent claude --agent codex` in a terminal") {
			t.Fatalf("print %v tty %v: %d calls:\n%s", c.print, c.tty, calls, out)
		}
		if _, err := os.Stat(filepath.Join(e.home, ".claude", "skills", "rex")); !os.IsNotExist(err) {
			t.Fatalf("print %v tty %v: a Rex skill exists", c.print, c.tty)
		}
	}
}

// withAnswers gives e new answers.
func withAnswers(e setupEnv, answers string) setupEnv {
	e.in = bufio.NewReader(strings.NewReader(answers))
	return e
}

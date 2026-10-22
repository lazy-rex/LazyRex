package main

// Agent resume (docs/hub.md, "Agent resume"): a detached agent identity continues its conversation in
// a new pane, from its profile's resume template.

import (
	"errors"
	"fmt"
)

// validResume checks a profile's resume template. ${conversation_id} must appear; every other
// token is a declared arg. Tokens are whole arguments and never the program, as in the command.
func validResume(p Profile, declared map[string]bool) error {
	if p.Resume == nil {
		return nil
	}
	if p.Kind != "agent" {
		return errors.New("resume is only for agent profiles")
	}
	if len(p.Resume) == 0 || p.Resume[0] == "" {
		return errors.New("resume needs a program")
	}
	conv := false
	for i, a := range p.Resume {
		for _, m := range argToken.FindAllStringSubmatch(a, -1) {
			if a != m[0] || i == 0 {
				return fmt.Errorf("resume: %s must be a whole argument, and not the program", m[0])
			}
			if m[1] == "conversation_id" {
				conv = true
			} else if !declared[m[1]] {
				return fmt.Errorf("resume: %s is not a declared arg", m[0])
			}
		}
	}
	if !conv {
		return errors.New("resume must contain ${conversation_id}")
	}
	return nil
}

// resumeArgv is the spawn argv (the same environment cleanup) with the resume template as the
// command. The spawn's resolved args fill their tokens; the identity's conversation fills
// ${conversation_id}, never an arg.
func resumeArgv(s *spawnRow, prof Profile, resolved map[string]string, conversation string) []string {
	vals := make(map[string]string, len(resolved)+1)
	for k, v := range resolved {
		vals[k] = v
	}
	vals["conversation_id"] = conversation
	prof.Command = prof.Resume
	return spawnArgv(s, prof, vals)
}

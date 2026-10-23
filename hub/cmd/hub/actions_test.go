package main

import "testing"

// TestActionTable: each name has one row, and each agent face place (agentOrder) has one row; the
// goldens see a duplicate only after it changes a face, and this test names the row. Each covered
// read (coveredReads, the render contract with the SDK) is a plugin face row that h.dispatch runs
// for any caller or for a registered plugin.
func TestActionTable(t *testing.T) {
	names, places := map[string]bool{}, map[int]string{}
	for _, a := range hubActions {
		if names[a.name] {
			t.Errorf("%s has two rows", a.name)
		}
		names[a.name] = true
		if a.agentOrder == 0 {
			continue
		}
		if other, ok := places[a.agentOrder]; ok {
			t.Errorf("%s and %s have agentOrder %d", other, a.name, a.agentOrder)
		}
		places[a.agentOrder] = a.name
	}
	for name := range coveredReads {
		a := actionNamed(name)
		if a == nil || !a.plugin || a.run == nil || (a.caller != callerAny && a.caller != callerPlugin) {
			t.Errorf("coveredReads names %s, which is not a plugin face row of any caller or of a plugin: %+v", name, a)
		}
	}
}

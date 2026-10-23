package main

import (
	"encoding/json"
	"testing"
)

// decodeAndValidate reads a snapshot as the client does (hub.view.subscribe and hub.view.get):
// a decode error and a validation error both refuse the snapshot.
func decodeAndValidate(body string) error {
	var s Snapshot
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		return err
	}
	return s.Validate("v1", "/thread/42")
}

func snapshotJSON(history, extra string) string {
	return `{"schema_version":1,"view_id":"v1","route":"/thread/42","revision":"1",` + extra +
		`"tree":{"title":"Thread","nodes":[{"id":"history","type":"history","page_size":3,` + history +
		`"messages":[{"id":"a"},{"id":"b"}]}]}}`
}

func TestHistoryTotal(t *testing.T) {
	for _, c := range []struct {
		name, history string
		valid         bool
	}{
		{"absent", ``, true},
		{"zero is too small for two messages", `"total":0,`, false},
		{"equal to the messages", `"total":2,`, true},
		{"larger than the messages", `"total":900,`, true},
		{"negative", `"total":-1,`, false},
		{"fractional", `"total":2.5,`, false},
		{"string", `"total":"2",`, false},
	} {
		if err := decodeAndValidate(snapshotJSON(c.history, "")); (err == nil) != c.valid {
			t.Errorf("%s: err=%v, want valid=%t", c.name, err, c.valid)
		}
	}
	// An empty thread: total 0 is valid.
	empty := `{"schema_version":1,"view_id":"v1","route":"/thread/42","revision":"1","tree":{"title":"Thread","nodes":[{"id":"history","type":"history","page_size":3,"total":0,"messages":[]}]}}`
	if err := decodeAndValidate(empty); err != nil {
		t.Errorf("empty thread: %v", err)
	}
}

// TestHistoryTotalChecksOnlyItsNode: a later snapshot with a smaller total (a deletion) is valid.
// Validate has no state, so it never compares with an earlier snapshot or with printed messages.
func TestHistoryTotalChecksOnlyItsNode(t *testing.T) {
	for _, total := range []string{`"total":900,`, `"total":2,`} {
		if err := decodeAndValidate(snapshotJSON(total, "")); err != nil {
			t.Fatalf("%s: %v", total, err)
		}
	}
}

func TestSnapshotOpen(t *testing.T) {
	for _, c := range []struct {
		name, open string
		valid      bool
		full       bool
	}{
		{"absent", ``, true, false},
		{"empty", `"open":{},`, true, false},
		{"full true", `"open":{"full":true},`, true, true},
		{"full false", `"open":{"full":false},`, true, false},
		{"full not a boolean", `"open":{"full":"yes"},`, false, false},
		{"full null", `"open":{"full":null},`, false, false},
		{"full a number", `"open":{"full":1},`, false, false},
		{"open null is absence", `"open":null,`, true, false},
		{"another key", `"open":{"full":true,"page":2},`, false, false},
		{"not an object", `"open":true,`, false, false},
		{"a list", `"open":[],`, false, false},
	} {
		body := snapshotJSON("", c.open)
		err := decodeAndValidate(body)
		if (err == nil) != c.valid {
			t.Errorf("%s: err=%v, want valid=%t", c.name, err, c.valid)
			continue
		}
		if !c.valid {
			continue
		}
		var s Snapshot
		json.Unmarshal([]byte(body), &s)
		if full := s.Open != nil && s.Open.Full; full != c.full {
			t.Errorf("%s: full=%t, want %t", c.name, full, c.full)
		}
	}
}

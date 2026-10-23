package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// retiredLab is a lab hub.db with OLD tables (records, sql_returns, ui_tabs) after the "before"
// schema step, and the retired entries of H.
func retiredLab(t *testing.T, retired []retiredEntry) *rehearsal {
	t.Helper()
	h := labHubDB(t)
	db, err := h.hubDB(false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE records (ns TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, rev INTEGER NOT NULL, PRIMARY KEY (ns, key));
		CREATE TABLE sql_returns (plugin TEXT NOT NULL, seq INTEGER NOT NULL);
		CREATE TABLE ui_tabs (id TEXT PRIMARY KEY, label TEXT NOT NULL DEFAULT '', rev_known INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if err := h.schema("before"); err != nil {
		t.Fatal(err)
	}
	h.retired, h.retiredRec = retired, &relRetiredRec{Entries: retired}
	return h
}

// A table or column of OLD may go only when H's retired schema names it, and only by absence: a
// retired table or column that is still there keeps its OLD form. In a forward phase an
// unconditional entry that OLD had must be gone; after a rollback it may come back.
func TestReleaseSchemaRetired(t *testing.T) {
	records := retiredEntry{Table: "records", Conditional: true}
	sqlReturns := retiredEntry{Table: "sql_returns"}
	revKnown := retiredEntry{Table: "ui_tabs", Column: "rev_known"}
	for name, tc := range map[string]struct {
		retired []retiredEntry
		change  string
		back    bool
		want    string // "" passes
	}{
		"declared conditional drop":         {[]retiredEntry{records}, `DROP TABLE records`, false, ""},
		"conditional table stays":           {[]retiredEntry{records}, ``, false, ""},
		"conditional table stays altered":   {[]retiredEntry{records}, `ALTER TABLE records DROP COLUMN value; ALTER TABLE records ADD COLUMN value BLOB`, false, "a column of OLD changed or went: column records.value"},
		"undeclared drop":                   {[]retiredEntry{records}, `DROP TABLE bindings`, false, "a column of OLD changed or went: column bindings."},
		"no list":                           {nil, `DROP TABLE records`, false, "a column of OLD changed or went: column records."},
		"declared column drop":              {[]retiredEntry{revKnown}, `ALTER TABLE ui_tabs DROP COLUMN rev_known`, false, ""},
		"other column of the table":         {[]retiredEntry{revKnown}, `ALTER TABLE ui_tabs DROP COLUMN label`, false, "a column of OLD changed or went: column ui_tabs.label"},
		"column entry is not the table":     {[]retiredEntry{revKnown}, `DROP TABLE ui_tabs`, false, "a column of OLD changed or went: column ui_tabs.id"},
		"retired column stays retyped":      {[]retiredEntry{revKnown}, `ALTER TABLE ui_tabs DROP COLUMN rev_known; ALTER TABLE ui_tabs ADD COLUMN rev_known TEXT`, false, "a column of OLD changed or went: column ui_tabs.rev_known"},
		"unconditional drop":                {[]retiredEntry{sqlReturns}, `DROP TABLE sql_returns`, false, ""},
		"unconditional still there":         {[]retiredEntry{sqlReturns}, ``, false, "H declares the drop of sql_returns, but it is still there"},
		"unconditional column still there":  {[]retiredEntry{revKnown}, ``, false, "H declares the drop of ui_tabs.rev_known, but it is still there"},
		"unconditional back after rollback": {[]retiredEntry{sqlReturns}, ``, true, ""},
		"declared drop in a back phase":     {[]retiredEntry{records}, `DROP TABLE records`, true, ""},
		"undeclared drop in a back phase":   {[]retiredEntry{records}, `DROP TABLE sql_returns`, true, "a column of OLD changed or went: column sql_returns."},
	} {
		h := retiredLab(t, tc.retired)
		if tc.change != "" {
			db, err := h.hubDB(false)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec(tc.change)
			db.Close()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		err := h.schemaAt("forward", tc.back)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// retiredScript writes an executable shell script and returns its path.
func retiredScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hub-H")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

const retiredUnknownFlag = `echo "flag provided but not defined: -retired-schema" >&2; echo "Usage of hub-H:" >&2; echo "  -config string" >&2; exit 2`

// The export of the H binary: version 1 with valid entries is read; Go's unknown-flag exit of an
// older hub is "absent"; every other answer fails.
func TestReleaseRetiredExport(t *testing.T) {
	good := `{"version":1,"retired":[{"table":"ui_tabs","column":"rev_known","conditional":false},{"table":"records","column":"","conditional":true}]}`
	exp, out, absent, err := exportRetired(retiredScript(t, `echo '`+good+`'`), t.TempDir())
	if err != nil || absent || exp == nil || len(exp.Retired) != 2 || exp.Retired[1] != (retiredEntry{Table: "records", Conditional: true}) || strings.TrimSpace(string(out)) != good {
		t.Errorf("a good export: %+v %q %v %v", exp, out, absent, err)
	}
	if _, _, absent, err := exportRetired(retiredScript(t, retiredUnknownFlag), t.TempDir()); err != nil || !absent {
		t.Errorf("an older hub (unknown flag, exit 2): absent %v, %v", absent, err)
	}
	old := relRetiredTimeout
	relRetiredTimeout = 500 * time.Millisecond
	defer func() { relRetiredTimeout = old }()
	for name, body := range map[string]string{
		"other first line, exit 2":  `echo "flag provided but not defined: -other" >&2; exit 2`,
		"unknown-flag line, exit 1": `echo "flag provided but not defined: -retired-schema" >&2; exit 1`,
		"exit 0, no output":         `exit 0`,
		"version 2":                 `echo '{"version":2,"retired":[]}'`,
		"unknown field":             `echo '{"version":1,"retired":[{"table":"t","column":"","conditional":false,"x":1}]}'`,
		"duplicate":                 `echo '{"version":1,"retired":[{"table":"t","column":"","conditional":false},{"table":"t","column":"","conditional":true}]}'`,
		"bad table name":            `echo '{"version":1,"retired":[{"table":"T; DROP","column":"","conditional":false}]}'`,
		"bad column name":           `echo '{"version":1,"retired":[{"table":"t","column":"a b","conditional":false}]}'`,
		"trailing data":             `echo '{"version":1,"retired":[]} {}'`,
		"timeout":                   `sleep 5`,
		"writes in its directory":   `touch state.db; echo '{"version":1,"retired":[]}'`,
	} {
		if _, _, _, err := exportRetired(retiredScript(t, body), t.TempDir()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// loadRetired checks the provenance of the H binary, reads its export once, and refuses a binary
// that changed during the export.
func TestReleaseLoadRetired(t *testing.T) {
	newH := func() *rehearsal {
		return &rehearsal{lab: t.TempDir(), c: &relCandidate{To: relPin{Commit: "h-commit"}}, r: &releaser{out: &bytes.Buffer{}}}
	}
	good := `echo '{"version":1,"retired":[{"table":"records","column":"","conditional":true}]}'`
	h, bin := newH(), retiredScript(t, good)
	var provs []string
	if err := h.loadRetired(bin, func(b string) error { provs = append(provs, b); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(provs) != 1 || provs[0] != bin {
		t.Errorf("provenance checks %v, want one of %s", provs, bin)
	}
	rec := h.retiredRec
	if rec == nil || rec.Commit != "h-commit" || len(rec.BinSHA256) != 64 || len(rec.OutputSHA256) != 64 || rec.Absent || len(h.retired) != 1 || h.retired[0].Table != "records" {
		t.Errorf("the record %+v, retired %+v", rec, h.retired)
	}
	if err := h.loadRetired(retiredScript(t, `exit 1`), func(string) error { return nil }); err != nil || h.retiredRec != rec {
		t.Errorf("a second load reads again: %v", err)
	}
	h = newH()
	if err := h.loadRetired(retiredScript(t, retiredUnknownFlag), func(string) error { return nil }); err != nil || h.retiredRec == nil || !h.retiredRec.Absent || len(h.retired) != 0 {
		t.Errorf("an older H: %+v, %v", h.retiredRec, err)
	}
	h = newH()
	if err := h.loadRetired(bin, func(string) error { return relFail("hub-H is built from x, not from h-commit") }); err == nil || !strings.Contains(err.Error(), "not from h-commit") || h.retiredRec != nil {
		t.Errorf("a provenance failure: %v", err)
	}
	h = newH()
	self := retiredScript(t, `echo "# changed" >> "$0"; `+good)
	if err := h.loadRetired(self, func(string) error { return nil }); err == nil || !strings.Contains(err.Error(), "changed during the export") || h.retiredRec != nil {
		t.Errorf("a binary that changed during the export: %v", err)
	}
}

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// relRetiredTimeout bounds the run of "hub-H -retired-schema".
var relRetiredTimeout = 30 * time.Second

// relRetiredName is the form of a table or column name in a retired entry.
var relRetiredName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// relUnknownRetiredFlag is the first stderr line of a hub without the export: Go's flag package
// (ExitOnError) prints it, then the usage, and exits 2.
const relUnknownRetiredFlag = "flag provided but not defined: -retired-schema"

// relRetiredRec records the -retired-schema export of the H binary that a hub rehearsal read: the
// commit, the sha256 of the binary and of the output, and the entries. Absent: the binary has no
// export (an older hub), so no OLD table or column may go.
type relRetiredRec struct {
	Commit       string         `json:"commit"`
	BinSHA256    string         `json:"bin_sha256"`
	OutputSHA256 string         `json:"output_sha256"`
	Absent       bool           `json:"absent"`
	Entries      []retiredEntry `json:"entries"`
}

// exportRetired runs "bin -retired-schema" in the empty directory dir, with PATH as its only
// variable. Exit 2 with the unknown-flag line first on stderr is an older hub: absent. Any other
// failure, an output that is not one valid version 1 document, or a file in dir afterwards fails.
func exportRetired(bin, dir string) (*retiredExport, []byte, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), relRetiredTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, bin, "-retired-schema")
	c.Dir, c.Env, c.WaitDelay = dir, []string{"PATH=" + os.Getenv("PATH")}, time.Second
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	if ctx.Err() != nil {
		return nil, nil, false, relFail("%s -retired-schema: no answer in %s", bin, relRetiredTimeout)
	}
	if ents, rerr := os.ReadDir(dir); rerr != nil || len(ents) != 0 {
		return nil, nil, false, relFail("%s -retired-schema wrote in its directory %s (%d entries, %v)", bin, dir, len(ents), rerr)
	}
	if err != nil {
		var ee *exec.ExitError
		first, _, _ := strings.Cut(stderr.String(), "\n")
		if errors.As(err, &ee) && ee.ExitCode() == 2 && first == relUnknownRetiredFlag {
			return nil, nil, true, nil
		}
		return nil, nil, false, relFail("%s -retired-schema: %v: %s", bin, err, strings.TrimSpace(stderr.String()))
	}
	d := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	d.DisallowUnknownFields()
	var exp retiredExport
	if err := d.Decode(&exp); err != nil {
		return nil, nil, false, relFail("the -retired-schema output of %s: %v", bin, err)
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, nil, false, relFail("the -retired-schema output of %s has data after the document", bin)
	}
	if exp.Version != 1 {
		return nil, nil, false, relFail("the -retired-schema output of %s has version %d, not 1", bin, exp.Version)
	}
	seen := map[retiredEntry]bool{}
	for _, e := range exp.Retired {
		if !relRetiredName.MatchString(e.Table) || (e.Column != "" && !relRetiredName.MatchString(e.Column)) {
			return nil, nil, false, relFail("the -retired-schema output of %s has the entry %+v, not a table or column name", bin, e)
		}
		k := retiredEntry{Table: e.Table, Column: e.Column}
		if seen[k] {
			return nil, nil, false, relFail("the -retired-schema output of %s names %s twice", bin, e.name())
		}
		seen[k] = true
	}
	return &exp, stdout.Bytes(), false, nil
}

// loadRetired reads the export of the H binary bin once: prov checks its provenance first (commit
// To, a clean build), and the binary must have the same sha256 after the export.
func (h *rehearsal) loadRetired(bin string, prov func(string) error) error {
	if h.retiredRec != nil {
		return nil
	}
	if err := prov(bin); err != nil {
		return err
	}
	before, err := sha256File(bin)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(h.lab, "retired-")
	if err != nil {
		return err
	}
	exp, out, absent, err := exportRetired(bin, dir)
	if err != nil {
		return err
	}
	after, err := sha256File(bin)
	if err != nil {
		return err
	}
	if after != before {
		return relFail("%s changed during the export (sha256 %s, then %s)", bin, before, after)
	}
	rec := &relRetiredRec{Commit: h.c.To.Commit, BinSHA256: before, Absent: absent, Entries: []retiredEntry{}}
	if !absent {
		sum := sha256.Sum256(out)
		rec.OutputSHA256, rec.Entries = hex.EncodeToString(sum[:]), exp.Retired
	}
	h.retired, h.retiredRec = rec.Entries, rec
	h.say("retired schema of H (%s, sha256 %s): absent %v, %d entries %v", rec.Commit, before, absent, len(rec.Entries), rec.Entries)
	return nil
}

// ensureRetired loads the retired schema of <run>/bin/hub-H when it exists. Without that binary
// the list stays empty: no OLD table or column may go.
func (h *rehearsal) ensureRetired() error {
	bin := filepath.Join(h.run, "bin", "hub-H")
	if h.retiredRec != nil || h.run == "" {
		return nil
	}
	if _, err := os.Stat(bin); err != nil {
		return nil
	}
	return h.loadRetired(bin, func(b string) error {
		return h.withLab(func(lr *releaser) error { return lr.checkProvenance(b, h.c.To.Commit, "hub-H retired-schema") })
	})
}

func (e retiredEntry) name() string {
	if e.Column == "" {
		return e.Table
	}
	return e.Table + "." + e.Column
}

// schemaColumn is the table and column of a schema item "column <table>.<name> ...".
func schemaColumn(item string) (string, string) {
	tc, _, _ := strings.Cut(strings.TrimPrefix(item, "column "), " ")
	t, c, _ := strings.Cut(tc, ".")
	return t, c
}

// hasRetired reports whether items have a column of e: any column of the table for a whole-table
// entry, else that column.
func hasRetired(items []string, e retiredEntry) bool {
	for _, x := range items {
		if !strings.HasPrefix(x, "column ") {
			continue
		}
		if t, c := schemaColumn(x); t == e.Table && (e.Column == "" || c == e.Column) {
			return true
		}
	}
	return false
}

// retiredGone reports whether the OLD column item x is gone as a retired entry allows: the entry
// names its table (whole) or its column, and that table or column is absent from items. A retired
// table or column that is still there keeps its OLD form, so it is never covered.
func (h *rehearsal) retiredGone(x string, items []string) bool {
	t, c := schemaColumn(x)
	for _, e := range h.retired {
		if e.Table == t && (e.Column == "" || e.Column == c) && !hasRetired(items, e) {
			return true
		}
	}
	return false
}

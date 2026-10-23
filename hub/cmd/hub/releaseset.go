package main

// The set file of a release-set rehearsal: the plugin releases that follow a hub release, in live
// order. Each entry names its FROM and TO commits, the seed calls (lab-made data that FROM writes
// with its own actions), the probes (calls with exact expected values), the row keys of its tables,
// the allowed value changes and inserts, and the owner review that binds all of it.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

type relSetFile struct {
	Plugins []relSetEntry `json:"plugins"`
}

type relSetEntry struct {
	Plugin  string              `json:"plugin"`
	From    string              `json:"from"`
	To      string              `json:"to"`
	Seed    []relSetItem        `json:"seed"`
	Probe   []relSetItem        `json:"probe"`
	Keys    map[string][]string `json:"keys"`
	Changes []relSetChange      `json:"changes"`
	Inserts []relSetInsert      `json:"inserts"`
	Review  *relSetReview       `json:"review"`
	hash    string              // the sha256 of the canonical entry without "review"
}

// relSetItem is one seed or probe item. Kind "action": a registered lab agent calls the plugin
// action Action with Args. Kind "view": the user path; a lab pane in PaneDir runs the palette
// Command, a lab renderer attaches the view at Route and sends the node action Action with Input and
// OperationKey. Kind "sql" (probe only): a read-only query of the lab plugin database, whose Rows
// must be exactly equal. Expect applies to the action or view operation result.
type relSetItem struct {
	Kind         string               `json:"kind"`
	Action       string               `json:"action,omitempty"`
	Args         json.RawMessage      `json:"args,omitempty"`
	PaneDir      string               `json:"pane_dir,omitempty"`
	Command      string               `json:"command,omitempty"`
	Route        string               `json:"route,omitempty"`
	NodeID       string               `json:"node_id,omitempty"`
	Input        json.RawMessage      `json:"input,omitempty"`
	OperationKey string               `json:"operation_key,omitempty"`
	SQL          string               `json:"sql,omitempty"`
	Params       []json.RawMessage    `json:"params,omitempty"`
	Rows         *[][]json.RawMessage `json:"rows,omitempty"`
	Expect       []relSetExpect       `json:"expect,omitempty"`
}

// relSetExpect is one assertion on a result: the value at Pointer is exactly Value, or, with Empty,
// an empty object or an empty array (Lua encodes an empty table as either).
type relSetExpect struct {
	Pointer string          `json:"pointer"`
	Value   json.RawMessage `json:"value,omitempty"`
	Empty   bool            `json:"empty,omitempty"`
}

// relSetChange allows one changed value: the row of table with key, in column. Expect is the exact
// new value, or {"sql": "<boolean expression over old and new>"}.
type relSetChange struct {
	Table  string                     `json:"table"`
	Key    map[string]json.RawMessage `json:"key"`
	Column string                     `json:"column"`
	Expect json.RawMessage            `json:"expect"`
}

// relSetInsert allows up to Max new rows in table. Each invariant is a SELECT that returns zero rows
// when the data is correct.
type relSetInsert struct {
	Table      string   `json:"table"`
	Max        int      `json:"max"`
	Invariants []string `json:"invariants"`
}

// relSetReview is the owner review of one entry: who, the message, and the entry hash it reviewed.
type relSetReview struct {
	By          string `json:"by"`
	MsgID       string `json:"msg_id"`
	EntrySHA256 string `json:"entry_sha256"`
	From        string `json:"from"`
	To          string `json:"to"`
}

var (
	relHexCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
	relSetName   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// noDupNames refuses a JSON text in which one object has two members with the same name (after
// the escapes are decoded). A decoder keeps only the last of them, so the other value would be
// lost without a sign.
func noDupNames(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	type frame struct {
		names map[string]bool // nil: an array
		name  bool            // an object expects a member name next
	}
	var st []*frame
	for {
		t, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var top *frame
		if len(st) > 0 {
			top = st[len(st)-1]
		}
		if top != nil && top.names != nil && top.name {
			if s, ok := t.(string); ok {
				if top.names[s] {
					return fmt.Errorf("an object has two members named %q", s)
				}
				top.names[s] = true
				top.name = false
				continue
			}
		}
		switch t {
		case json.Delim('{'):
			st = append(st, &frame{names: map[string]bool{}, name: true})
			continue
		case json.Delim('['):
			st = append(st, &frame{})
			continue
		case json.Delim('}'), json.Delim(']'):
			st = st[:len(st)-1]
			if len(st) > 0 {
				top = st[len(st)-1]
			} else {
				top = nil
			}
		}
		// A value ended: an object now expects a member name.
		if top != nil && top.names != nil {
			top.name = true
		}
	}
}

// canonicalJSON is the sorted, compact form of a JSON value, with numbers as written and no HTML
// escapes: the form of `jq -cS`. Two members with one name are an error.
func canonicalJSON(raw []byte) ([]byte, error) {
	if err := noDupNames(raw); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, fmt.Errorf("more than one JSON value")
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// setEntryHash is the sha256 of the canonical JSON of one raw entry without its "review" field.
func setEntryHash(raw json.RawMessage) (string, error) {
	_, h, err := setEntryCanonical(raw)
	return h, err
}

// setEntryCanonical is the canonical JSON of one raw entry without its "review" field, and its sha256.
func setEntryCanonical(raw json.RawMessage) ([]byte, string, error) {
	// Check the original bytes: the decode below keeps one of two members with one name.
	if err := noDupNames(raw); err != nil {
		return nil, "", err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return nil, "", err
	}
	delete(m, "review")
	b, err := json.Marshal(m)
	if err != nil {
		return nil, "", err
	}
	c, err := canonicalJSON(b)
	if err != nil {
		return nil, "", err
	}
	s := sha256.Sum256(c)
	return c, hex.EncodeToString(s[:]), nil
}

// parseSet reads a set file and checks every rule that needs no lab: the fields, the names, the
// commits, the probes, the changes and inserts, and the review hash of each entry. An unknown field
// is refused (4.2 has no "destructive" field).
func parseSet(b []byte) (*relSetFile, error) {
	if err := noDupNames(b); err != nil {
		return nil, relFail("the set file: %v", err)
	}
	var raw struct {
		Plugins []json.RawMessage `json:"plugins"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&raw); err != nil {
		return nil, relFail("the set file: %v", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, relFail("the set file has more data after its JSON object")
	}
	if len(raw.Plugins) == 0 {
		return nil, relFail("the set file names no plugin")
	}
	set := &relSetFile{}
	seen, keys := map[string]bool{}, map[string]bool{}
	for i, r := range raw.Plugins {
		var e relSetEntry
		d := json.NewDecoder(bytes.NewReader(r))
		d.DisallowUnknownFields()
		if err := d.Decode(&e); err != nil {
			return nil, relFail("the set file, entry %d: %v", i+1, err)
		}
		if err := e.check(); err != nil {
			return nil, relFail("the set file, entry %d (%s): %v", i+1, e.Plugin, err)
		}
		if seen[e.Plugin] {
			return nil, relFail("the set file names %s twice", e.Plugin)
		}
		seen[e.Plugin] = true
		for _, it := range append(slices.Clone(e.Seed), e.Probe...) {
			if it.OperationKey == "" {
				continue
			}
			if keys[it.OperationKey] {
				return nil, relFail("the set file uses the operation_key %q twice: each view operation needs its own key", it.OperationKey)
			}
			keys[it.OperationKey] = true
		}
		h, err := setEntryHash(r)
		if err != nil {
			return nil, relFail("the set file, entry %d: %v", i+1, err)
		}
		e.hash = h
		if e.Review.EntrySHA256 != h {
			return nil, relFail("the set file, entry %d (%s): the review is of entry %s, but the entry is %s now", i+1, e.Plugin, e.Review.EntrySHA256, h)
		}
		set.Plugins = append(set.Plugins, e)
	}
	return set, nil
}

// check checks one entry without its hash.
func (e *relSetEntry) check() error {
	switch {
	case !relSetName.MatchString(e.Plugin):
		return fmt.Errorf("the plugin name %q is not allowed", e.Plugin)
	case !relHexCommit.MatchString(e.From) || !relHexCommit.MatchString(e.To):
		return fmt.Errorf("from and to must be 40 lowercase hex digits")
	case e.From == e.To:
		return fmt.Errorf("from and to are the same commit: not a release step")
	case len(e.Probe) == 0:
		return fmt.Errorf("no probe")
	case e.Review == nil:
		return fmt.Errorf("no review")
	case e.Review.By == "" || e.Review.MsgID == "":
		return fmt.Errorf("the review names no reviewer or message")
	case e.Review.From != e.From || e.Review.To != e.To:
		return fmt.Errorf("the review is of %s..%s, not %s..%s", e.Review.From, e.Review.To, e.From, e.To)
	}
	runs := 0
	for i, it := range e.Seed {
		if it.Kind == "sql" {
			return fmt.Errorf("seed %d: a sql item is a probe only", i+1)
		}
		if err := it.check(); err != nil {
			return fmt.Errorf("seed %d: %v", i+1, err)
		}
	}
	for i, it := range e.Probe {
		if err := it.check(); err != nil {
			return fmt.Errorf("probe %d: %v", i+1, err)
		}
		if it.Kind != "sql" {
			runs++
			if len(it.Expect) == 0 {
				return fmt.Errorf("probe %d (%s) has no expected value", i+1, it.Action)
			}
		}
	}
	if runs == 0 {
		return fmt.Errorf("no probe of kind action or view: a sql probe alone runs no plugin code")
	}
	for t, cols := range e.Keys {
		if t == "" || len(cols) == 0 || strings.HasPrefix(strings.ToLower(t), "lazyrex_") {
			return fmt.Errorf("the keys of table %q", t)
		}
		for _, c := range cols {
			if c == "" || strings.EqualFold(c, "rowid") {
				return fmt.Errorf("the keys of table %q: %q is not a stable key column", t, c)
			}
		}
	}
	for i, c := range e.Changes {
		if c.Table == "" || c.Column == "" || len(c.Key) == 0 || len(c.Expect) == 0 {
			return fmt.Errorf("change %d: it needs a table, a key, a column and an expected value", i+1)
		}
		var sqlForm map[string]json.RawMessage
		if json.Unmarshal(c.Expect, &sqlForm) == nil {
			if s, ok := sqlForm["sql"]; !ok || len(sqlForm) != 1 || len(s) < 3 {
				return fmt.Errorf(`change %d: an object expect must be {"sql": "<expression>"}`, i+1)
			}
		}
	}
	for i, n := range e.Inserts {
		if n.Table == "" || n.Max < 1 || len(n.Invariants) == 0 {
			return fmt.Errorf("insert %d: it needs a table, a max of 1 or more, and invariants", i+1)
		}
		for _, q := range n.Invariants {
			if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(q)), "SELECT ") {
				return fmt.Errorf("insert %d: the invariant %q is not a SELECT", i+1, q)
			}
		}
	}
	return nil
}

// check checks one seed or probe item by its kind.
func (it *relSetItem) check() error {
	isObject := func(raw json.RawMessage) bool {
		var m map[string]json.RawMessage
		return len(raw) == 0 || json.Unmarshal(raw, &m) == nil
	}
	switch it.Kind {
	case "action":
		if it.Action == "" || strings.ContainsAny(it.Action, " \t\n") || strings.HasPrefix(it.Action, "hub.") {
			return fmt.Errorf("the action %q is not a plugin action", it.Action)
		}
		if !isObject(it.Args) {
			return fmt.Errorf("the args of %s are not a JSON object", it.Action)
		}
		if it.PaneDir != "" || it.Command != "" || it.Route != "" || it.NodeID != "" || len(it.Input) > 0 || it.OperationKey != "" || it.SQL != "" || it.Rows != nil {
			return fmt.Errorf("an action item has only action, args and expect")
		}
	case "view":
		switch {
		case !strings.HasPrefix(it.PaneDir, "<LAB>/"):
			return fmt.Errorf("the pane_dir %q is not a lab directory (<LAB>/...)", it.PaneDir)
		case it.Command == "" || !strings.HasPrefix(it.Route, "/") || it.NodeID == "" || it.Action == "":
			return fmt.Errorf("a view item needs a command, a route, a node_id and an action")
		case it.OperationKey == "":
			return fmt.Errorf("a view item needs an operation_key")
		case !isObject(it.Input):
			return fmt.Errorf("the input of %s/%s is not a JSON object", it.NodeID, it.Action)
		case len(it.Args) > 0 || it.SQL != "" || it.Rows != nil:
			return fmt.Errorf("a view item has no args, sql or rows")
		}
	case "sql":
		switch {
		case !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(it.SQL)), "SELECT "):
			return fmt.Errorf("the sql %q is not a SELECT", it.SQL)
		case it.Rows == nil:
			return fmt.Errorf("a sql item needs rows (an empty list for no row)")
		case it.Action != "" || len(it.Args) > 0 || it.PaneDir != "" || it.Command != "" || len(it.Expect) > 0:
			return fmt.Errorf("a sql item has only sql, params and rows")
		}
	default:
		return fmt.Errorf("the kind %q is not action, view or sql", it.Kind)
	}
	for _, x := range it.Expect {
		if x.Pointer != "" && !strings.HasPrefix(x.Pointer, "/") {
			return fmt.Errorf("%q is not a JSON pointer", x.Pointer)
		}
		if (len(x.Value) == 0) == !x.Empty {
			return fmt.Errorf("the pointer %q needs exactly one of value and empty", x.Pointer)
		}
	}
	return nil
}

// releaseSetHash is `lazyrex-hub release set-hash -set <file>`: it reads nothing but the file and
// writes, for each entry, its plugin, its entry_sha256 and its canonical bytes (the bytes the hash
// covers). An owner review quotes that hash. It reports an entry that breaks a rule, except a
// missing or stale review: the review comes after this step.
func releaseSetHash(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("release set-hash", flag.ContinueOnError)
	set.SetOutput(stderr)
	file := set.String("set", "", "the set file")
	if err := set.Parse(args); err != nil || *file == "" || set.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: lazyrex-hub release set-hash -set <file>")
		return relUsage
	}
	b, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(stderr, "release set-hash: %v\n", err)
		return relFailed
	}
	if err := noDupNames(b); err != nil {
		fmt.Fprintf(stderr, "release set-hash: the set file: %v\n", err)
		return relFailed
	}
	var raw struct {
		Plugins []json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		fmt.Fprintf(stderr, "release set-hash: the set file: %v\n", err)
		return relFailed
	}
	if len(raw.Plugins) == 0 {
		fmt.Fprintln(stderr, "release set-hash: the set file has no plugins")
		return relFailed
	}
	code := relOK
	for i, r := range raw.Plugins {
		var e relSetEntry
		d := json.NewDecoder(bytes.NewReader(r))
		d.DisallowUnknownFields()
		err := d.Decode(&e)
		c, h, herr := setEntryCanonical(r)
		if herr != nil {
			fmt.Fprintf(stderr, "release set-hash: entry %d: %v\n", i+1, herr)
			code = relFailed
			continue
		}
		if err == nil {
			e.Review = &relSetReview{By: "-", MsgID: "-", From: e.From, To: e.To}
			err = e.check()
		}
		if err != nil {
			fmt.Fprintf(stderr, "release set-hash: entry %d (%s): %v\n", i+1, e.Plugin, err)
			code = relFailed
		}
		fmt.Fprintf(stdout, "plugin %s entry_sha256 %s\n%s\n", e.Plugin, h, c)
	}
	return code
}

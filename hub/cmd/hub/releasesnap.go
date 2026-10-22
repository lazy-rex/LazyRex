package main

// The data snapshots of a release-set rehearsal (plan r3, section 1). A snapshot holds the schema
// and the typed value of every row of a plugin's SQL database and of its hub store namespace
// (records and ops of the lab hub.db), keyed by stable row keys, never by rowid. A check compares
// the current snapshot with the reference: the snapshot after the seed, plus the first value of
// each row that an allowed insert added later.

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// relCell is one typed SQLite value: i (integer), r (real), t (text), b (blob, hex) or n (NULL).
type relCell struct {
	T byte   `json:"t"`
	V string `json:"v"`
}

func cellOf(v any) (relCell, error) {
	switch x := v.(type) {
	case nil:
		return relCell{T: 'n'}, nil
	case int64:
		return relCell{T: 'i', V: strconv.FormatInt(x, 10)}, nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return relCell{}, fmt.Errorf("a real value that is not finite")
		}
		return relCell{T: 'r', V: strconv.FormatFloat(x, 'g', -1, 64)}, nil
	case string:
		return relCell{T: 't', V: x}, nil
	case []byte:
		return relCell{T: 'b', V: hex.EncodeToString(x)}, nil
	}
	return relCell{}, fmt.Errorf("a value of type %T", v)
}

// jsonValue is the JSON form of a cell for an expect: a number (a real always has a "." or an
// exponent, so 1 and 1.0 differ), a string or null. A blob has no JSON form.
func (c relCell) jsonValue() (string, error) {
	switch c.T {
	case 'n':
		return "null", nil
	case 'i':
		return c.V, nil
	case 'r':
		if !strings.ContainsAny(c.V, ".e") {
			return c.V + ".0", nil
		}
		return c.V, nil
	case 't':
		b, err := json.Marshal(c.V)
		return string(b), err
	}
	return "", fmt.Errorf("a blob value has no JSON form")
}

// sqlValue is the value to bind for a cell.
func (c relCell) sqlValue() any {
	switch c.T {
	case 'i':
		n, _ := strconv.ParseInt(c.V, 10, 64)
		return n
	case 'r':
		f, _ := strconv.ParseFloat(c.V, 64)
		return f
	case 't':
		return c.V
	case 'b':
		b, _ := hex.DecodeString(c.V)
		return b
	}
	return nil
}

// relSnapTable is one table: its columns in order, its key columns, and its rows by key.
type relSnapTable struct {
	Columns []string             `json:"columns"`
	Key     []string             `json:"key"`
	Rows    map[string][]relCell `json:"rows"` // by the JSON of the key cells
}

// relSnap is one snapshot of a plugin's data.
type relSnap struct {
	UserVersion int64                    `json:"user_version"`
	Objects     map[string]string        `json:"objects"` // "<type> <name>": its sql (indexes, triggers, views; not the hub's)
	Tables      map[string]*relSnapTable `json:"tables"`  // the SQL tables; "hub:records" and "hub:ops" for the store
	added       map[string]int           // reference only: the rows that allowed inserts added, by table
}

// rowKey is the JSON of the key cells of a row; a NULL key cell is an error.
func rowKey(t *relSnapTable, row []relCell) (string, error) {
	var key []relCell
	for _, k := range t.Key {
		i := slices.Index(t.Columns, k)
		if i < 0 {
			return "", fmt.Errorf("the key column %s is not a column", k)
		}
		if row[i].T == 'n' {
			return "", fmt.Errorf("a row has a NULL in the key column %s", k)
		}
		key = append(key, row[i])
	}
	b, err := json.Marshal(key)
	return string(b), err
}

// tableKey is the stable key of table: the entry's keys, else its declared PRIMARY KEY when that is
// not the rowid (a single INTEGER PRIMARY KEY column is the rowid).
func tableKey(db *sql.DB, table string, keys map[string][]string) (cols, key []string, err error) {
	rows, err := db.Query(`SELECT name, type, pk FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	type pkCol struct {
		name string
		pk   int
	}
	var pks []pkCol
	intPK := false
	for rows.Next() {
		var name, typ string
		var pk int
		if err := rows.Scan(&name, &typ, &pk); err != nil {
			return nil, nil, err
		}
		cols = append(cols, name)
		if pk > 0 {
			pks = append(pks, pkCol{name, pk})
			intPK = strings.EqualFold(strings.TrimSpace(typ), "INTEGER")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if k, ok := keys[table]; ok {
		for _, c := range k {
			if !slices.Contains(cols, c) {
				return nil, nil, fmt.Errorf("table %s has no key column %s", table, c)
			}
		}
		return cols, k, nil
	}
	if len(pks) == 0 || (len(pks) == 1 && intPK) {
		return nil, nil, fmt.Errorf("table %s has no stable key: name its key columns in \"keys\"", table)
	}
	sort.Slice(pks, func(i, j int) bool { return pks[i].pk < pks[j].pk })
	for _, p := range pks {
		key = append(key, p.name)
	}
	return cols, key, nil
}

// readTable reads every row of a query into t by key; a duplicate key is an error.
func readTable(db *sql.DB, t *relSnapTable, name, query string, args ...any) error {
	rows, err := db.Query(query, args...)
	if err != nil {
		return fmt.Errorf("table %s: %v", name, err)
	}
	defer rows.Close()
	t.Rows = map[string][]relCell{}
	for rows.Next() {
		vals := make([]any, len(t.Columns))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("table %s: %v", name, err)
		}
		row := make([]relCell, len(vals))
		for i, v := range vals {
			if row[i], err = cellOf(v); err != nil {
				return fmt.Errorf("table %s, column %s: %v", name, t.Columns[i], err)
			}
		}
		k, err := rowKey(t, row)
		if err != nil {
			return fmt.Errorf("table %s: %v", name, err)
		}
		if _, dup := t.Rows[k]; dup {
			return fmt.Errorf("table %s: the key %s does not identify one row", name, k)
		}
		t.Rows[k] = row
	}
	return rows.Err()
}

// quoteIdent quotes an SQL identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// snapPluginDB reads a plugin database (read-only) into s. The lazyrex_* tables are the hub's and
// are not read.
func snapPluginDB(db *sql.DB, keys map[string][]string, s *relSnap) error {
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&s.UserVersion); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND lower(name) NOT LIKE 'lazyrex\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	s.Objects = map[string]string{}
	obj, err := db.Query(`SELECT type, name, sql FROM sqlite_master WHERE type IN ('index', 'trigger', 'view') AND sql IS NOT NULL
		AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND lower(tbl_name) NOT LIKE 'lazyrex\_%' ESCAPE '\'`)
	if err != nil {
		return err
	}
	for obj.Next() {
		var typ, name, text string
		if err := obj.Scan(&typ, &name, &text); err != nil {
			obj.Close()
			return err
		}
		s.Objects[typ+" "+name] = text
	}
	obj.Close()
	if err := obj.Err(); err != nil {
		return err
	}
	for _, n := range names {
		cols, key, err := tableKey(db, n, keys)
		if err != nil {
			return err
		}
		t := &relSnapTable{Columns: cols, Key: key}
		q := make([]string, len(cols))
		for i, c := range cols {
			q[i] = quoteIdent(c)
		}
		if err := readTable(db, t, n, "SELECT "+strings.Join(q, ", ")+" FROM "+quoteIdent(n)); err != nil {
			return err
		}
		s.Tables[n] = t
	}
	return nil
}

// snapStore reads the records and ops of namespace ns of a hub.db (read-only) into s.
func snapStore(db *sql.DB, ns string, s *relSnap) error {
	for _, st := range []struct{ name, query string }{
		{"hub:records", "SELECT key, value, rev FROM records WHERE ns = ?"},
		{"hub:ops", "SELECT key, fingerprint, result FROM ops WHERE ns = ?"},
	} {
		cols := []string{"key", "value", "rev"}
		if st.name == "hub:ops" {
			cols = []string{"key", "fingerprint", "result"}
		}
		t := &relSnapTable{Columns: cols, Key: []string{"key"}}
		if err := readTable(db, t, st.name, st.query, ns); err != nil {
			return err
		}
		s.Tables[st.name] = t
	}
	return nil
}

// newSnap reads one snapshot: the plugin database (nil when the plugin has none) and the store
// namespace of the plugin in the hub database.
func newSnap(pluginDB, hubDB *sql.DB, ns string, keys map[string][]string) (*relSnap, error) {
	s := &relSnap{Tables: map[string]*relSnapTable{}}
	if pluginDB != nil {
		if err := snapPluginDB(pluginDB, keys, s); err != nil {
			return nil, err
		}
	} else if len(keys) > 0 {
		return nil, fmt.Errorf("the keys name tables, but the plugin has no database")
	}
	if err := snapStore(hubDB, ns, s); err != nil {
		return nil, err
	}
	return s, nil
}

// relSnapDiff is what a check found: the allowed changes and inserts, for the record.
type relSnapDiff struct {
	Changed  []string `json:"changed,omitempty"`  // "table key column"
	Inserted []string `json:"inserted,omitempty"` // "table key"
}

// checkSnap compares cur with the reference ref under the rules of entry e (plan r3, section 1),
// then adds the newly inserted rows to ref. eval evaluates a changes predicate with old and new
// bound. A lost table, column or row always fails. A changed value needs a changes entry for that
// row and column whose expect matches. A new row needs an inserts entry for its table; the rows
// added since the reference was made count against its max.
func checkSnap(ref, cur *relSnap, e *relSetEntry, eval func(expr string, old, new relCell) (bool, error)) (*relSnapDiff, error) {
	d := &relSnapDiff{}
	if ref.added == nil {
		ref.added = map[string]int{}
	}
	if cur.UserVersion < ref.UserVersion {
		return nil, relFail("user_version went down from %d to %d", ref.UserVersion, cur.UserVersion)
	}
	names := make([]string, 0, len(ref.Tables))
	for n := range ref.Tables {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rt, ct := ref.Tables[n], cur.Tables[n]
		if ct == nil {
			return nil, relFail("table %s is gone", n)
		}
		for _, c := range rt.Columns {
			if !slices.Contains(ct.Columns, c) {
				return nil, relFail("column %s.%s is gone", n, c)
			}
		}
		if !slices.Equal(rt.Key, ct.Key) {
			return nil, relFail("the key of table %s changed from %v to %v", n, rt.Key, ct.Key)
		}
		keys := make([]string, 0, len(rt.Rows))
		for k := range rt.Rows {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			crow, ok := ct.Rows[k]
			if !ok {
				return nil, relFail("table %s lost the row %s", n, k)
			}
			for i, c := range rt.Columns {
				old, now := rt.Rows[k][i], crow[slices.Index(ct.Columns, c)]
				if old == now {
					continue
				}
				if err := allowedChange(e, n, rt, k, c, old, now, eval); err != nil {
					return nil, err
				}
				d.Changed = append(d.Changed, n+" "+k+" "+c)
			}
		}
	}
	for name, text := range ref.Objects {
		now, ok := cur.Objects[name]
		if !ok {
			return nil, relFail("the schema object %s is gone", name)
		}
		if now != text {
			return nil, relFail("the schema object %s changed from %q to %q", name, text, now)
		}
	}
	cnames := make([]string, 0, len(cur.Tables))
	for n := range cur.Tables {
		cnames = append(cnames, n)
	}
	sort.Strings(cnames)
	for _, n := range cnames {
		ct, rt := cur.Tables[n], ref.Tables[n]
		var fresh []string
		for k := range ct.Rows {
			if rt == nil || rt.Rows[k] == nil {
				fresh = append(fresh, k)
			}
		}
		if len(fresh) == 0 {
			continue
		}
		sort.Strings(fresh)
		ins := setInsertFor(e, n)
		if ins == nil {
			return nil, relFail("table %s has %d new rows (first %s), and no inserts entry allows them", n, len(fresh), fresh[0])
		}
		if ref.added[n]+len(fresh) > ins.Max {
			return nil, relFail("table %s has %d rows more than after the seed; its inserts entry allows %d", n, ref.added[n]+len(fresh), ins.Max)
		}
		for _, k := range fresh {
			d.Inserted = append(d.Inserted, n+" "+k)
		}
	}
	// The check passed. The reference learns the new state: a higher user_version, new schema
	// objects, new columns (with the current values of the reference rows) and new rows (with their
	// first values). So a later drop of any of them fails.
	ref.UserVersion = cur.UserVersion
	for name, text := range cur.Objects {
		if _, ok := ref.Objects[name]; !ok {
			if ref.Objects == nil {
				ref.Objects = map[string]string{}
			}
			ref.Objects[name] = text
		}
	}
	for _, n := range cnames {
		ct := cur.Tables[n]
		rt := ref.Tables[n]
		if rt == nil {
			rt = &relSnapTable{Columns: slices.Clone(ct.Columns), Key: ct.Key, Rows: map[string][]relCell{}}
			ref.Tables[n] = rt
		}
		for ci, c := range ct.Columns {
			if slices.Contains(rt.Columns, c) {
				continue
			}
			rt.Columns = append(rt.Columns, c)
			for k, row := range rt.Rows {
				rt.Rows[k] = append(row, ct.Rows[k][ci])
			}
		}
		for k, row := range ct.Rows {
			if rt.Rows[k] == nil {
				ref.added[n]++
				rt.Rows[k] = projectRow(ct, rt, row)
			}
		}
	}
	return d, nil
}

// projectRow gives row (of table ct) in the column order of rt.
func projectRow(ct, rt *relSnapTable, row []relCell) []relCell {
	out := make([]relCell, len(rt.Columns))
	for i, c := range rt.Columns {
		out[i] = row[slices.Index(ct.Columns, c)]
	}
	return out
}

func setInsertFor(e *relSetEntry, table string) *relSetInsert {
	for i := range e.Inserts {
		if e.Inserts[i].Table == table {
			return &e.Inserts[i]
		}
	}
	return nil
}

// allowedChange finds the changes entry for (table, key, column) and checks its expect.
func allowedChange(e *relSetEntry, table string, t *relSnapTable, key, column string, old, now relCell, eval func(string, relCell, relCell) (bool, error)) error {
	for _, c := range e.Changes {
		if c.Table != table || c.Column != column {
			continue
		}
		k, err := changeKey(t, c.Key)
		if err != nil {
			return relFail("the changes entry for %s.%s: %v", table, column, err)
		}
		if k != key {
			continue
		}
		var sqlForm struct {
			SQL string `json:"sql"`
		}
		if json.Unmarshal(c.Expect, &sqlForm) == nil && sqlForm.SQL != "" {
			ok, err := eval(sqlForm.SQL, old, now)
			if err != nil {
				return relFail("the changes predicate of %s %s %s: %v", table, key, column, err)
			}
			if !ok {
				return relFail("%s %s %s changed from %s to %s, and the changes predicate is false", table, key, column, old.V, now.V)
			}
			return nil
		}
		want, err := canonicalJSON(c.Expect)
		if err != nil {
			return relFail("the changes expect of %s %s %s: %v", table, key, column, err)
		}
		got, err := now.jsonValue()
		if err != nil {
			return relFail("%s %s %s: %v", table, key, column, err)
		}
		if got != string(want) {
			return relFail("%s %s %s is %s, not the expected %s", table, key, column, got, want)
		}
		return nil
	}
	return relFail("%s %s %s changed from %q to %q, and no changes entry allows it", table, key, column, old.V, now.V)
}

// changeKey is the row key of a changes entry: its key values as cells, in the table's key order.
// The entry must name exactly the key columns.
func changeKey(t *relSnapTable, key map[string]json.RawMessage) (string, error) {
	if len(key) != len(t.Key) {
		return "", fmt.Errorf("the key names %d columns; the table key is %v", len(key), t.Key)
	}
	var cells []relCell
	for _, k := range t.Key {
		raw, ok := key[k]
		if !ok {
			return "", fmt.Errorf("the key has no column %s", k)
		}
		c, err := cellOfJSON(raw)
		if err != nil {
			return "", fmt.Errorf("the key column %s: %v", k, err)
		}
		cells = append(cells, c)
	}
	b, err := json.Marshal(cells)
	return string(b), err
}

// cellOfJSON is the cell of a JSON key value: a string is text, an integer number is an integer.
func cellOfJSON(raw json.RawMessage) (relCell, error) {
	if strings.TrimSpace(string(raw)) == "null" {
		return relCell{}, fmt.Errorf("a key value cannot be null")
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return relCell{T: 't', V: s}, nil
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil {
		return relCell{T: 'i', V: strconv.FormatInt(n, 10)}, nil
	}
	return relCell{}, fmt.Errorf("%s is not a text or an integer key value", raw)
}

// checkInvariants runs every inserts invariant of e on db; a returned row fails.
func checkInvariants(db *sql.DB, e *relSetEntry) error {
	for _, n := range e.Inserts {
		for _, q := range n.Invariants {
			rows, err := db.Query(q)
			if err != nil {
				return relFail("the invariant %q: %v", q, err)
			}
			bad := rows.Next()
			err = rows.Err()
			rows.Close()
			if err != nil {
				return relFail("the invariant %q: %v", q, err)
			}
			if bad {
				return relFail("the invariant %q returns a row", q)
			}
		}
	}
	return nil
}

// sqlPredicate evaluates a changes predicate in db with :old and :new bound to the typed values.
func sqlPredicate(db *sql.DB) func(expr string, old, new relCell) (bool, error) {
	return func(expr string, old, new relCell) (bool, error) {
		var ok sql.NullBool
		err := db.QueryRow("SELECT ("+expr+") IS 1", sql.Named("old", old.sqlValue()), sql.Named("new", new.sqlValue())).Scan(&ok)
		return ok.Valid && ok.Bool, err
	}
}

// checkKeyTables fails when keys name a table that the reference snapshot s does not have (a typo
// would otherwise check nothing). Later snapshots do not need it: a table that is gone fails the
// compare.
func checkKeyTables(s *relSnap, keys map[string][]string) error {
	for k := range keys {
		if s.Tables[k] == nil {
			return fmt.Errorf("the keys name table %s, which the database does not have", k)
		}
	}
	return nil
}

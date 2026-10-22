package main

// Plugin SQL databases (docs/sql.md): one SQLite file for each plugin name, a writer and a reader
// connection with the hub's authorizer, the value encoding and one plugin statement.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Limits (docs/sql.md section 5).
const (
	sqlTextMax     = 32 << 10 // SQLITE_LIMIT_SQL_LENGTH
	sqlParamsMax   = 999      // SQLITE_LIMIT_VARIABLE_NUMBER
	sqlValueMax    = 1 << 20  // SQLITE_LIMIT_LENGTH
	sqlPagesMax    = 262144   // max_page_count: 1 GiB of 4 KiB pages
	sqlStmtTimeout = 5 * time.Second
	// sqlReplyBudget is the encoded result size that a reply may use: the reply guard limit
	// (1 047 552 bytes, observed) less room for the reply envelope.
	sqlReplyBudget = 1_040_000
	maxExactInt    = 1 << 53 // a Lua number holds integers exactly up to 2^53
)

// The SQLite limit IDs that sqlite.Limit takes.
const (
	limitLength         = 0
	limitSQLLength      = 1
	limitAttached       = 7
	limitVariableNumber = 9
)

// hubTables are the plugin file's objects that the hub owns (lower case).
var hubTables = map[string]bool{"lazyrex_ops": true, "lazyrex_ops_at": true, "lazyrex_notice_outbox": true}

const sqlHubSchema = `CREATE TABLE IF NOT EXISTS lazyrex_ops (key TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, reply TEXT NOT NULL, at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS lazyrex_ops_at ON lazyrex_ops(at);
CREATE TABLE IF NOT EXISTS lazyrex_notice_outbox (seq INTEGER PRIMARY KEY AUTOINCREMENT, agent TEXT NOT NULL, pending INTEGER NOT NULL)`

// readPragmas are the pragmas that a plugin may run; user_version is the only one that it may
// also write.
var readPragmas = map[string]bool{"user_version": true, "foreign_keys": true, "table_info": true, "table_xinfo": true,
	"index_list": true, "index_info": true, "index_xinfo": true, "foreign_key_list": true, "foreign_key_check": true,
	"integrity_check": true, "quick_check": true}

// argPragmas take a name argument, not a value: a non-empty argument is no write.
var argPragmas = map[string]bool{"table_info": true, "table_xinfo": true, "index_list": true, "index_info": true,
	"index_xinfo": true, "foreign_key_list": true, "foreign_key_check": true, "integrity_check": true, "quick_check": true}

func reservedName(name string) bool { return strings.HasPrefix(strings.ToLower(name), "lazyrex_") }

// sqlConn is one connection to a plugin file, with the hub's authorizer. The caller serializes
// its use: the authorizer state belongs to the statement that runs.
type sqlConn struct {
	c        *sql.Conn
	db       *sql.DB
	trusted  bool            // the hub's own statements: the policy does not apply
	readOnly bool            // a hub.sql.query statement: any write is refused
	denied   []string        // the reasons of the current statement's refusals
	writes   map[string]bool // tables that the current statement may write (lower case)
	reads    map[string]bool // tables that the current statement reads (lower case)
	ddl      bool            // the current statement changes the schema
}

func (k *sqlConn) authorize(a sqlite.AuthorizerActionCode, a1, a2, db, trig string) sqlite.AuthorizerReturnCode {
	if k.trusted {
		return sqlite.AuthorizerOK
	}
	deny := func(why string) sqlite.AuthorizerReturnCode {
		k.denied = append(k.denied, why)
		return sqlite.AuthorizerDeny
	}
	queryOnly := func() bool { return k.readOnly }
	switch a {
	case sqlite.AuthAttach, sqlite.AuthDetach:
		return deny("ATTACH and DETACH are refused")
	case sqlite.AuthTransaction, sqlite.AuthSavepoint:
		return deny("the hub owns the transactions")
	case sqlite.AuthCreateVTable, sqlite.AuthDropVTable:
		return deny("virtual tables are refused")
	case sqlite.AuthPragma:
		p := strings.ToLower(a1)
		if !readPragmas[p] {
			return deny("PRAGMA " + a1 + " is refused")
		}
		if a2 != "" && !argPragmas[p] {
			if p != "user_version" {
				return deny("PRAGMA " + a1 + " is read-only")
			}
			if queryOnly() {
				return deny("hub.sql.query is read-only")
			}
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthFunction:
		if strings.EqualFold(a2, "load_extension") {
			return deny("load_extension is refused")
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthInsert, sqlite.AuthUpdate, sqlite.AuthDelete:
		if reservedName(a1) {
			return deny(a1 + " is the hub's")
		}
		if queryOnly() {
			return deny("hub.sql.query is read-only")
		}
		if !strings.HasPrefix(strings.ToLower(a1), "sqlite_") {
			k.writes[strings.ToLower(a1)] = true
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthRead:
		if reservedName(a1) {
			return deny(a1 + " is the hub's")
		}
		k.reads[strings.ToLower(a1)] = true
		return sqlite.AuthorizerOK
	case sqlite.AuthCreateTable, sqlite.AuthCreateTempTable, sqlite.AuthCreateView, sqlite.AuthCreateTempView,
		sqlite.AuthDropTable, sqlite.AuthDropTempTable, sqlite.AuthDropView, sqlite.AuthDropTempView:
		k.ddl = true
		if queryOnly() {
			return deny("hub.sql.query is read-only")
		}
		if reservedName(a1) {
			return deny(a1 + " is the hub's")
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthCreateIndex, sqlite.AuthCreateTempIndex, sqlite.AuthCreateTrigger, sqlite.AuthCreateTempTrigger,
		sqlite.AuthDropIndex, sqlite.AuthDropTempIndex, sqlite.AuthDropTrigger, sqlite.AuthDropTempTrigger:
		k.ddl = true
		if queryOnly() {
			return deny("hub.sql.query is read-only")
		}
		if reservedName(a1) || reservedName(a2) {
			return deny("the names that start with lazyrex_ are the hub's")
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthAlterTable:
		k.ddl = true
		if queryOnly() {
			return deny("hub.sql.query is read-only")
		}
		if reservedName(a2) {
			return deny(a2 + " is the hub's")
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthAnalyze, sqlite.AuthReindex:
		if queryOnly() {
			return deny("hub.sql.query is read-only")
		}
	}
	return sqlite.AuthorizerOK // SELECT, RECURSIVE
}

// openSQLConn opens one connection to path with the fixed settings (docs/sql.md section 2).
func openSQLConn(ctx context.Context, path string, reader bool) (*sqlConn, error) {
	dsn := "file:" + path + "?_defensive=1&_pragma=foreign_keys(1)&_pragma=trusted_schema(0)&_pragma=journal_mode(wal)" +
		"&_pragma=synchronous(full)&_pragma=busy_timeout(5000)&_pragma=max_page_count(" + strconv.Itoa(sqlPagesMax) + ")"
	if reader {
		dsn += "&_pragma=query_only(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	c, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	k := &sqlConn{c: c, db: db, writes: map[string]bool{}, reads: map[string]bool{}}
	for _, l := range [][2]int{{limitAttached, 0}, {limitSQLLength, sqlTextMax}, {limitVariableNumber, sqlParamsMax}, {limitLength, sqlValueMax}} {
		if _, err := sqlite.Limit(c, l[0], l[1]); err != nil {
			k.close()
			return nil, err
		}
	}
	err = c.Raw(func(dc any) error {
		r, ok := dc.(sqlite.AuthorizerRegisterer)
		if !ok {
			return errors.New("the SQLite driver has no authorizer")
		}
		return r.RegisterAuthorizer(k.authorize)
	})
	if err != nil {
		k.close()
		return nil, err
	}
	return k, nil
}

func (k *sqlConn) close() {
	k.c.Close()
	k.db.Close()
}

// hub runs one of the hub's own statements on the connection, outside the policy.
func (k *sqlConn) hub(ctx context.Context, q string, args ...any) error {
	k.trusted = true
	defer func() { k.trusted = false }()
	_, err := k.c.ExecContext(ctx, q, args...)
	return err
}

// inTx reports whether the connection is in a transaction: BEGIN fails inside one. Any other answer
// counts as no transaction, and a transaction that the probe began is rolled back at once.
func (k *sqlConn) inTx(ctx context.Context) bool {
	err := k.hub(ctx, "BEGIN")
	if err != nil && strings.Contains(err.Error(), "within a transaction") {
		return true
	}
	if err == nil {
		k.hub(context.WithoutCancel(ctx), "ROLLBACK")
	}
	return false
}

func (k *sqlConn) hubInt(ctx context.Context, q string) (int64, error) {
	k.trusted = true
	defer func() { k.trusted = false }()
	var n int64
	err := k.c.QueryRowContext(ctx, q).Scan(&n)
	return n, err
}

// postDDL refuses a schema change that made an object with a reserved name, or a column with a
// time type. SQLite reports some of its own schema writes before the CREATE action, so the hub
// checks the schema after the statement. The driver turns TEXT in a DATE, DATETIME or TIMESTAMP
// column into a Go time and loses the text (modernc.org/sqlite v1.61.0, rows.go), so the hub
// refuses those declared types: a plugin declares a time as TEXT or INTEGER.
func (k *sqlConn) postDDL(ctx context.Context) error {
	k.trusted = true
	defer func() { k.trusted = false }()
	var bad []string
	for _, schema := range []string{"main", "temp"} {
		rows, err := k.c.QueryContext(ctx, `SELECT name FROM `+schema+`.sqlite_schema WHERE lower(name) LIKE 'lazyrex\_%' ESCAPE '\'`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n string
			rows.Scan(&n)
			if schema == "temp" || !hubTables[strings.ToLower(n)] { // the hub's names live in main only
				bad = append(bad, schema+"."+n)
			}
		}
		rows.Close()
	}
	if len(bad) > 0 {
		return refuse("refused", "the names that start with lazyrex_ are the hub's: %s", strings.Join(bad, ", "))
	}
	for _, schema := range []string{"main", "temp"} {
		rows, err := k.c.QueryContext(ctx, `SELECT m.name, c.name, c.type FROM `+schema+`.sqlite_schema m, pragma_table_xinfo(m.name, ?) c
			WHERE m.type = 'table' AND upper(trim(c.type)) IN ('DATE', 'DATETIME', 'TIMESTAMP')`, schema)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t, c, ty string
			rows.Scan(&t, &c, &ty)
			bad = append(bad, fmt.Sprintf("%s.%s %s", t, c, ty))
		}
		rows.Close()
	}
	if len(bad) > 0 {
		return refuse("refused", "the column types DATE, DATETIME and TIMESTAMP are refused (use TEXT or INTEGER): %s", strings.Join(bad, ", "))
	}
	return nil
}

// oneStatement refuses SQL text with more than one statement: the driver runs every statement of
// the text, also in a query. sqlite3_complete finds the end of the first statement (it knows
// literals, comments and trigger bodies); after it only blanks and comments may follow.
func oneStatement(q string) error {
	if !strings.Contains(q, ";") {
		return nil
	}
	tls := libc.NewTLS()
	defer tls.Close()
	for i := 0; i < len(q); i++ {
		if q[i] != ';' {
			continue
		}
		cs, err := libc.CString(q[:i+1])
		if err != nil {
			return err
		}
		done := sqlite3.Xsqlite3_complete(tls, cs) != 0
		libc.Xfree(tls, cs)
		if done {
			if !onlyBlanks(q[i+1:]) {
				return refuse("invalid_input", "one statement for each call: text after the first statement")
			}
			return nil
		}
	}
	return nil
}

// onlyBlanks reports whether s has only white space and SQL comments.
func onlyBlanks(s string) bool {
	for i := 0; i < len(s); {
		switch {
		case s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\f' || s[i] == ';':
			i++
		case strings.HasPrefix(s[i:], "--"):
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				return true
			}
			i += j + 1
		case strings.HasPrefix(s[i:], "/*"):
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return true // SQLite reads an open comment to the end
			}
			i += j + 4
		default:
			return false
		}
	}
	return true
}

// sqlParams decodes the params of a call: a JSON array for ? parameters, an object for :name
// parameters. An empty object or array, or no params, is no parameters (Lua encodes an empty table
// as {}).
func sqlParams(v any) ([]any, error) {
	var out []any
	switch p := v.(type) {
	case nil:
		return nil, nil
	case []any:
		for i, x := range p {
			val, err := sqlValue(x)
			if err != nil {
				return nil, refuse("invalid_input", "params[%d]: %v", i+1, err)
			}
			out = append(out, val)
		}
	case map[string]any:
		for name, x := range p {
			if name == "" || strings.ContainsAny(name, ":@$") {
				return nil, refuse("invalid_input", "a parameter name is the name without its prefix: %q", name)
			}
			val, err := sqlValue(x)
			if err != nil {
				return nil, refuse("invalid_input", "params.%s: %v", name, err)
			}
			out = append(out, sql.Named(name, val))
		}
	default:
		return nil, refuse("invalid_input", "params must be an array or an object")
	}
	if len(out) > sqlParamsMax {
		return nil, refuse("too_large", "at most %d parameters", sqlParamsMax)
	}
	return out, nil
}

// sqlValue decodes one parameter value (docs/sql.md section 4).
func sqlValue(x any) (any, error) {
	switch v := x.(type) {
	case nil:
		return nil, nil
	case bool:
		if v {
			return int64(1), nil
		}
		return int64(0), nil
	case float64:
		if v == math.Trunc(v) && !math.IsInf(v, 0) {
			if math.Abs(v) > maxExactInt {
				return nil, errors.New(`an integer above 2^53 is not exact in Lua: send {"int": "<decimal>"}`)
			}
			return int64(v), nil
		}
		return v, nil
	case string:
		if len(v) > sqlValueMax {
			return nil, errors.New("a value is at most 1 MiB")
		}
		return v, nil
	case map[string]any:
		if len(v) != 1 {
			return nil, errors.New(`a tagged value has one key: "int", "blob" or "null"`)
		}
		if n, ok := v["null"]; ok {
			if n != true {
				return nil, errors.New(`the null tag is {"null": true}`)
			}
			return nil, nil
		}
		if s, ok := v["int"].(string); ok {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil, errors.New("the int tag is a decimal int64")
			}
			return n, nil
		}
		if s, ok := v["blob"].(string); ok {
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return nil, errors.New("the blob tag is standard base64")
			}
			if len(b) > sqlValueMax {
				return nil, errors.New("a value is at most 1 MiB")
			}
			return b, nil
		}
		return nil, errors.New(`a tagged value has one key: "int", "blob" or "null"`)
	}
	return nil, fmt.Errorf("unsupported value %T", x)
}

// sqlNull is the tag of NULL in a result: Rex Lua drops a JSON null in an array (observed on Rex
// 0.0.1792158932-g88fd41).
var sqlNull = map[string]any{"null": true}

// resultValue encodes one result value (docs/sql.md section 4).
func resultValue(x any) (any, error) {
	switch v := x.(type) {
	case nil:
		return sqlNull, nil
	case int64:
		if v > maxExactInt || v < -maxExactInt {
			return map[string]any{"int": strconv.FormatInt(v, 10)}, nil
		}
		return v, nil
	case float64:
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return nil, refuse("unsupported_value", "an infinite REAL has no JSON form")
		}
		return v, nil
	case string:
		if !utf8.ValidString(v) {
			return map[string]any{"blob": base64.StdEncoding.EncodeToString([]byte(v))}, nil
		}
		return v, nil
	case []byte:
		return map[string]any{"blob": base64.StdEncoding.EncodeToString(v)}, nil
	}
	return nil, refuse("unsupported_value", "unsupported result value %T", x)
}

// stmtResult is the outcome of one plugin statement.
type stmtResult struct {
	columns  []string
	rows     [][]any
	complete bool
	size     int // the encoded size of rows
	changes  int64
	id       int64
	changed  int64           // total_changes() during the statement: trigger and cascade writes count
	writes   map[string]bool // write candidates of the authorizer
	reads    map[string]bool
	ddl      bool
}

// marks are the tables that the statement changed: the write candidates, only when
// total_changes() moved. A schema change marks every view ("*").
func (r *stmtResult) marks() []string {
	if r.ddl {
		return []string{"*"}
	}
	if r.changed == 0 {
		return nil
	}
	var out []string
	for t := range r.writes {
		out = append(out, t)
	}
	return out
}

// sqlError turns a driver error of a plugin statement into a reply error.
func (k *sqlConn) sqlError(ctx context.Context, err error) error {
	if len(k.denied) > 0 {
		return refuse("refused", "%s", strings.Join(k.denied, "; "))
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return refuse("timeout", "the statement took more than %s", sqlStmtTimeout)
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		name := sqlite.ErrorCodeString[se.Code()]
		switch se.Code() & 0xff {
		case sqlite3.SQLITE_TOOBIG:
			return refuse("too_large", "%s", err)
		case sqlite3.SQLITE_FULL:
			return refuse("too_large", "the database file is full (1 GiB): %s", err)
		case sqlite3.SQLITE_INTERRUPT:
			return refuse("timeout", "the statement took more than %s", sqlStmtTimeout)
		}
		return refuse("sql_error", "%s: %s", name, err)
	}
	return refuse("sql_error", "%s", err)
}

// run runs one plugin statement on the connection, in the caller's transaction. budget is the
// reply size that the rows may use; a query stops at it (complete false), and an exec with
// RETURNING fails with too_large.
func (k *sqlConn) run(ctx context.Context, q string, params []any, readOnly bool, budget int) (*stmtResult, error) {
	if len(q) > sqlTextMax {
		return nil, refuse("too_large", "the SQL text is at most %d bytes", sqlTextMax)
	}
	if strings.TrimSpace(q) == "" {
		return nil, refuse("invalid_input", "sql is required")
	}
	if err := oneStatement(q); err != nil {
		return nil, err
	}
	before, err := k.hubInt(ctx, "SELECT total_changes()")
	if err != nil {
		return nil, err
	}
	k.denied, k.writes, k.reads, k.ddl, k.readOnly = nil, map[string]bool{}, map[string]bool{}, false, readOnly
	defer func() { k.readOnly = false }()
	sctx, cancel := context.WithTimeout(ctx, sqlStmtTimeout)
	defer cancel()
	rows, err := k.c.QueryContext(sctx, q, params...)
	if err != nil {
		return nil, k.sqlError(sctx, err)
	}
	r := &stmtResult{complete: true}
	r.columns, _ = rows.Columns()
	vals := make([]any, len(r.columns))
	ptrs := make([]any, len(r.columns))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			rows.Close()
			return nil, k.sqlError(sctx, err)
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			if row[i], err = resultValue(v); err != nil {
				rows.Close()
				return nil, err
			}
		}
		b, _ := json.Marshal(row)
		if r.size+len(b)+1 > budget {
			if !readOnly || len(r.rows) == 0 {
				rows.Close()
				return nil, refuse("too_large", `{"row": %d, "bytes": %d}: the rows exceed the reply limit`, len(r.rows), r.size+len(b))
			}
			r.complete = false
			break
		}
		r.size += len(b) + 1
		r.rows = append(r.rows, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, k.sqlError(sctx, err)
	}
	if !r.complete && !readOnly {
		return nil, refuse("too_large", "the RETURNING rows exceed the reply limit")
	}
	r.writes, r.reads, r.ddl = k.writes, k.reads, k.ddl
	if r.ddl {
		if err := k.postDDL(ctx); err != nil {
			return nil, err
		}
	}
	k.trusted = true
	err = k.c.QueryRowContext(ctx, "SELECT changes(), last_insert_rowid(), total_changes()").Scan(&r.changes, &r.id, &r.changed)
	k.trusted = false
	if err != nil {
		return nil, err
	}
	r.changed -= before
	if r.changed == 0 {
		r.changes = 0 // changes() keeps the count of the last DML statement; this one changed nothing
	}
	return r, nil
}

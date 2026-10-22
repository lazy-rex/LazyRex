// Package sqlpolicy is the draft 2 prototype of the hub.sql statement policy (docs/sql.md,
// section 6). Run: cd lab/probes/sqlpolicy && go test -v .
package sqlpolicy

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"modernc.org/sqlite"
)

// conn is one plugin database connection with the hub's authorizer.
type conn struct {
	c       *sql.Conn
	trusted bool            // the hub's own statements: the policy does not apply
	denied  []string        // the reasons of the current statement's refusals
	writes  map[string]bool // tables that the current statement may write (lower case)
	ddl     bool            // the current statement changes the schema
}

// pragmas that a plugin may read; user_version is the only one that it may also write.
var readPragmas = map[string]bool{"user_version": true, "foreign_keys": true, "table_info": true, "table_xinfo": true,
	"index_list": true, "index_info": true, "index_xinfo": true, "foreign_key_list": true, "foreign_key_check": true,
	"integrity_check": true, "quick_check": true}

// argPragmas take a name argument, not a value: a non-empty arg2 is no write.
var argPragmas = map[string]bool{"table_info": true, "table_xinfo": true, "index_list": true, "index_info": true,
	"index_xinfo": true, "foreign_key_list": true, "foreign_key_check": true, "integrity_check": true, "quick_check": true}

func reserved(name string) bool { return strings.HasPrefix(strings.ToLower(name), "lazyrex_") }

func (k *conn) authorize(a sqlite.AuthorizerActionCode, a1, a2, db, trig string) sqlite.AuthorizerReturnCode {
	if k.trusted {
		return sqlite.AuthorizerOK
	}
	deny := func(why string) sqlite.AuthorizerReturnCode {
		k.denied = append(k.denied, why)
		return sqlite.AuthorizerDeny
	}
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
		if a2 != "" && !argPragmas[p] && p != "user_version" {
			return deny("PRAGMA " + a1 + " is read-only")
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthFunction:
		if strings.EqualFold(a2, "load_extension") {
			return deny("load_extension is refused")
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthInsert, sqlite.AuthUpdate, sqlite.AuthDelete:
		if reserved(a1) {
			return deny(a1 + " is the hub's")
		}
		if !strings.HasPrefix(strings.ToLower(a1), "sqlite_") {
			k.writes[strings.ToLower(a1)] = true
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthRead:
		if reserved(a1) {
			return deny(a1 + " is the hub's")
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthCreateTable, sqlite.AuthCreateTempTable, sqlite.AuthCreateView, sqlite.AuthCreateTempView,
		sqlite.AuthDropTable, sqlite.AuthDropTempTable, sqlite.AuthDropView, sqlite.AuthDropTempView:
		k.ddl = true
		if reserved(a1) {
			return deny(a1 + " is the hub's")
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthCreateIndex, sqlite.AuthCreateTempIndex, sqlite.AuthCreateTrigger, sqlite.AuthCreateTempTrigger,
		sqlite.AuthDropIndex, sqlite.AuthDropTempIndex, sqlite.AuthDropTrigger, sqlite.AuthDropTempTrigger:
		k.ddl = true
		if reserved(a1) || reserved(a2) {
			return deny("the hub's names are reserved")
		}
		return sqlite.AuthorizerOK
	case sqlite.AuthAlterTable:
		k.ddl = true
		if reserved(a2) {
			return deny(a2 + " is the hub's")
		}
		return sqlite.AuthorizerOK
	}
	return sqlite.AuthorizerOK // SELECT, RECURSIVE, ANALYZE, REINDEX
}

func open(ctx context.Context, path string) (*conn, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_defensive=1&_pragma=foreign_keys(1)&_pragma=trusted_schema(0)&_pragma=journal_mode(wal)")
	if err != nil {
		return nil, err
	}
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	k := &conn{c: c, writes: map[string]bool{}}
	if _, err := sqlite.Limit(c, 7 /* SQLITE_LIMIT_ATTACHED */, 0); err != nil {
		return nil, err
	}
	err = c.Raw(func(dc any) error { return dc.(sqlite.AuthorizerRegisterer).RegisterAuthorizer(k.authorize) })
	return k, err
}

// result of one plugin statement: the error, the total changes and the tables to mark.
type result struct {
	err     error
	changed int64
	marks   []string // nil: no mark; ["*"]: a schema change marks every view
}

func (k *conn) totalChanges(ctx context.Context) int64 {
	k.trusted = true
	defer func() { k.trusted = false }()
	var n int64
	k.c.QueryRowContext(ctx, "SELECT total_changes()").Scan(&n)
	return n
}

// reservedObjects lists schema objects with a reserved name that the hub did not make.
func (k *conn) reservedObjects(ctx context.Context, hubOwned map[string]bool) []string {
	k.trusted = true
	defer func() { k.trusted = false }()
	var out []string
	for _, schema := range []string{"main", "temp"} {
		rows, err := k.c.QueryContext(ctx, `SELECT name FROM `+schema+`.sqlite_schema WHERE lower(name) LIKE 'lazyrex\_%' ESCAPE '\'`)
		if err != nil {
			out = append(out, "error: "+err.Error())
			continue
		}
		for rows.Next() {
			var n string
			rows.Scan(&n)
			if !hubOwned[strings.ToLower(n)] {
				out = append(out, schema+"."+n)
			}
		}
		rows.Close()
	}
	return out
}

// exec runs one plugin statement inside the caller's transaction.
func (k *conn) exec(ctx context.Context, q string, hubOwned map[string]bool) result {
	k.denied, k.writes, k.ddl = nil, map[string]bool{}, false
	before := k.totalChanges(ctx)
	_, err := k.c.ExecContext(ctx, q)
	if err != nil {
		if len(k.denied) > 0 {
			return result{err: fmt.Errorf("refused: %s", strings.Join(k.denied, "; "))}
		}
		return result{err: err}
	}
	r := result{changed: k.totalChanges(ctx) - before}
	if k.ddl {
		if bad := k.reservedObjects(ctx, hubOwned); len(bad) > 0 {
			return result{err: fmt.Errorf("refused: reserved names: %s", strings.Join(bad, ", "))}
		}
		r.marks = []string{"*"}
		return r
	}
	if r.changed > 0 {
		for t := range k.writes {
			r.marks = append(r.marks, t)
		}
	}
	return r
}

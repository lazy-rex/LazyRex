package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sqlFixture is startFixture with fix.sql: it performs one hub.sql action as the plugin and replies
// {result, error, n, third}. n and third are read in Lua from the first row of a query result, so
// the test sees what a Rex Lua plugin sees.
var sqlFixture = strings.NewReplacer("  {name = 'fix.reload',", "  {name = 'fix.sql', kind = 'client', title = 'SQL', args = {type = 'object'}},\n  {name = 'fix.reload',").Replace(strings.Replace(fmt.Sprintf(startFixture, "sq"), `  elseif ev.action == 'fix.reload' then`, `  elseif ev.action == 'fix.sql' then
    local res, err = rex.call('client.perform', {client_id = hub_id, action = ev.args.action, args = ev.args.args, timeout_ms = 20000})
    local out = {result = res and res.result, error = err and (type(err) == 'table' and tostring(err.message) or tostring(err)) or nil}
    local rows = out.result and out.result.rows
    if type(rows) == 'table' and rows[1] then out.n, out.third = #rows[1], rows[1][3] end
    rex.call('client.reply', {call_id = ev.call_id, result = out})
    return
  elseif ev.action == 'fix.reload' then`, 1))

type sqlReply struct {
	Result map[string]any `json:"result"`
	Error  string         `json:"error"`
	N      int            `json:"n"`
	Third  any            `json:"third"`
}

// sqlCall performs one hub.sql action as the plugin sq.
func (s *startLab) sqlCall(plugin, action string, args string) sqlReply {
	s.t.Helper()
	var a map[string]any
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		s.t.Fatalf("bad test args %s", args)
	}
	id := s.status(plugin).ClientID
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	raw, err := s.c.perform(ctx, id, "fix.sql", map[string]any{"action": action, "args": a})
	if err != nil {
		s.t.Fatalf("fix.sql %s: %v", action, err)
	}
	var r sqlReply
	json.Unmarshal(raw, &r)
	return r
}

// hub.sql through a Rex Lua plugin: the NULL tag keeps the row positions (Rex Lua drops a JSON
// null in an array), a large integer keeps its digits, a handler transaction commits, and the
// harness reads and writes the database with hub.test.sql.
func TestSQLWireLab(t *testing.T) {
	s := newStartLab(t, "sq")
	s.writeSource("sq", `{"name":"sq","main":"main.lua"}`, "")
	os.WriteFile(filepath.Join(s.src, "sq", "main.lua"), []byte(sqlFixture), 0o600)
	s.start()
	s.startRun("sq")
	ok := func(action, args string) map[string]any {
		t.Helper()
		r := s.sqlCall("sq", action, args)
		if r.Error != "" {
			t.Fatalf("%s %s: %s", action, args, r.Error)
		}
		return r.Result
	}
	ok("hub.sql.exec", `{"sql":"CREATE TABLE t (a, b, c)"}`)
	ok("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (?, ?, ?)", "params":[1, {"null":true}, 3]}`)
	r := s.sqlCall("sq", "hub.sql.query", `{"sql":"SELECT a, b, c FROM t", "params":{}}`)
	if r.Error != "" || r.N != 3 || r.Third != 3.0 {
		t.Fatalf("a NULL in the middle: n %d, third %v, %v %s", r.N, r.Third, r.Result, r.Error)
	}
	// A NULL parameter at the end.
	ok("hub.sql.exec", `{"sql":"DELETE FROM t"}`)
	ok("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (?, ?, ?)", "params":[{"int":"9007199254740993"}, 2, {"null":true}]}`)
	got := ok("hub.sql.query", `{"sql":"SELECT a, b, c FROM t"}`)
	if b, _ := json.Marshal(got["rows"]); string(b) != `[[{"int":"9007199254740993"},2,{"null":true}]]` {
		t.Fatalf("a NULL at the end and a large integer: %s", b)
	}
	// A handler transaction.
	tx := ok("hub.sql.begin", `{}`)["tx"].(string)
	ok("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (4, 5, 6)", "tx":"`+tx+`"}`)
	if rows := s.must("hub.test.sql", map[string]any{"plugin": "sq", "sql": "SELECT count(*) FROM t"})["rows"]; fmt.Sprint(rows) != "[[1]]" {
		t.Fatalf("an uncommitted write is visible: %v", rows)
	}
	ok("hub.sql.commit", `{"tx":"`+tx+`"}`)
	if rows := s.must("hub.test.sql", map[string]any{"plugin": "sq", "sql": "SELECT count(*) FROM t"})["rows"]; fmt.Sprint(rows) != "[[2]]" {
		t.Fatalf("after the commit: %v", rows)
	}
	// A test write, as another writer.
	s.must("hub.test.sql", map[string]any{"plugin": "sq", "sql": "INSERT INTO t VALUES (7, 8, 9)", "write": true})
	if rows := ok("hub.sql.query", `{"sql":"SELECT count(*) FROM t"}`)["rows"]; fmt.Sprint(rows) != "[[3]]" {
		t.Fatalf("after the test write: %v", rows)
	}
	// A commit fault: fail before the commit, then a lost reply after it.
	s.must("hub.test.inject", map[string]any{"plugin": "sq", "match": map[string]any{"sql_commit_writes": "t"}, "fail": "no", "fail_code": "unavailable"})
	if r := s.sqlCall("sq", "hub.sql.exec", `{"sql":"INSERT INTO t VALUES (0, 0, 0)"}`); !strings.Contains(r.Error, "no") {
		t.Fatalf("the fault before the commit: %+v", r)
	}
	s.must("hub.test.inject", map[string]any{"plugin": "sq", "match": map[string]any{"sql_commit_writes": "t"}, "fail": "lost", "after": true})
	if r := s.sqlCall("sq", "hub.sql.exec", `{"sql":"INSERT INTO t VALUES (1, 1, 1)"}`); !strings.Contains(r.Error, "lost") {
		t.Fatalf("the fault after the commit: %+v", r)
	}
	if rows := ok("hub.sql.query", `{"sql":"SELECT count(*) FROM t"}`)["rows"]; fmt.Sprint(rows) != "[[4]]" {
		t.Fatalf("after the faults (one row committed): %v", rows)
	}
	// Another writer at begin: the injected row commits before the plugin transaction begins, so the
	// transaction sees it.
	s.must("hub.test.inject", map[string]any{"plugin": "sq", "match": map[string]any{"sql_begin": true},
		"sql": []any{map[string]any{"sql": "INSERT INTO t VALUES (?, ?, ?)", "params": []any{10, 10, 10}}}})
	tx = ok("hub.sql.begin", `{}`)["tx"].(string)
	if rows := ok("hub.sql.query", `{"sql":"SELECT count(*) FROM t WHERE a = 10", "tx":"`+tx+`"}`)["rows"]; fmt.Sprint(rows) != "[[1]]" {
		t.Fatalf("the injected writer: %v", rows)
	}
	ok("hub.sql.rollback", `{"tx":"`+tx+`"}`)
	// A commit fault for one parameter: only the commit that inserts x1 fails.
	s.must("hub.test.inject", map[string]any{"plugin": "sq", "match": map[string]any{"sql_commit_writes": "t", "sql_param": "x1"}, "fail": "x1 fails"})
	ok("hub.sql.exec", `{"sql":"INSERT INTO t VALUES (?, 0, 0)", "params":["x0"]}`)
	if r := s.sqlCall("sq", "hub.sql.exec", `{"sql":"INSERT INTO t VALUES (?, 0, 0)", "params":["x1"]}`); !strings.Contains(r.Error, "x1 fails") {
		t.Fatalf("the parameter fault: %+v", r)
	}
	// An action fault for one parameter.
	s.must("hub.test.fault", map[string]any{"point": "action", "action": "hub.sql.query", "when": "before", "mode": "fail", "sql_param": "k9", "plugin": "sq"})
	ok("hub.sql.query", `{"sql":"SELECT count(*) FROM t WHERE a = ?", "params":["k8"]}`)
	if r := s.sqlCall("sq", "hub.sql.query", `{"sql":"SELECT count(*) FROM t WHERE a = ?", "params":["k9"]}`); r.Error == "" {
		t.Fatalf("the action fault for k9: %+v", r)
	}
	// The rollback: hub.admin.sql_return refuses a running plugin; after a stop it moves the file
	// aside, and the next start has a new, empty file.
	if _, err := s.call("hub.admin.sql_return", map[string]any{"plugin": "sq"}); err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Fatalf("a return of a running plugin: %v", err)
	}
	s.must("hub.admin.stop", map[string]any{"plugin": "sq"})
	ret := s.must("hub.admin.sql_return", map[string]any{"plugin": "sq"})
	dest, _ := ret["returned"].(string)
	if _, err := os.Stat(filepath.Join(dest, "sq.db")); err != nil {
		t.Fatalf("the returned file: %v (%v)", err, ret)
	}
	s.must("hub.admin.start", map[string]any{"plugin": "sq"})
	s.registered("sq")
	if r := s.sqlCall("sq", "hub.sql.query", `{"sql":"SELECT count(*) FROM t"}`); !strings.Contains(r.Error, "no such table") {
		t.Fatalf("the file after the return: %+v", r)
	}
}

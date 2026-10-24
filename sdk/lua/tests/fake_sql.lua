-- A fake hub.sql for the SDK tests (wire: hub.sql handoff v1 and the NULL tag {null = true}). It
-- keeps tables in memory, and it knows only the statements that the tests use: CREATE TABLE,
-- CREATE INDEX, ALTER TABLE ADD COLUMN, PRAGMA user_version, and INSERT, SELECT, UPDATE and DELETE
-- of the forms below. A handler transaction buffers its changes on a copy; commit installs the copy,
-- rollback drops it. Saved replies commit with the transaction. Every call is logged in fake.calls.
--   local fake = dofile('fake_sql.lua')()
--   fake.handle(action, args) -> result | nil, {code, message}
-- fake.fail_next[action] = {code, message} fails the next call of that action once.
-- fake.on(action, fn(args)) runs fn before the call (for example to make a commit fail).
-- fake.lose_reply[action] = {code, message}: the next call of that action runs, and its reply is this error.
-- fake.max_rows = n: a query reply holds at most n rows (complete = false, as over the hub's reply limit).
return function()
  local fake = {calls = {}, ops = {}, state = {tables = {}, user_version = 0}, open = nil, next_token = 0, fail_next = {}, lose_reply = {}, hooks = {}}
  local function copy(v)
    if type(v) ~= 'table' then return v end
    local r = {}
    for k, x in pairs(v) do r[k] = copy(x) end
    return r
  end
  local function err(code, message) return nil, {code = code, message = message} end
  local function value(p)
    if type(p) == 'table' and p.null == true then return nil end
    if p == true then return 1 elseif p == false then return 0 end
    return p
  end
  local function wire(v) if v == nil then return {null = true} end return v end
  local function trim(s) return (s:gsub('^%s+', ''):gsub('%s+$', '')) end

  -- run runs one statement on state db. It answers the reply or nil, error.
  local function run(db, sql, params)
    sql = trim(sql)
    params = params or {}
    local n = 0
    for _ in sql:gmatch('%?') do n = n + 1 end
    local i = 0
    local function nextp() i = i + 1; return value(params[i]) end
    local up = sql:upper()
    if up:match('^PRAGMA USER_VERSION$') then return {columns = {'user_version'}, rows = {{db.user_version}}, complete = true} end
    local v = up:match('^PRAGMA USER_VERSION = (%d+)$')
    if v then db.user_version = tonumber(v); return {changes = 0, id = 0} end
    local cname, cols = sql:match('^CREATE TABLE ([%w_]+) %((.*)%)$')
    if cname then
      if db.tables[cname] then return err('sql_error', 'SQLITE_ERROR: table ' .. cname .. ' already exists') end
      local t = {columns = {}, rows = {}, seq = 0}
      for def in (cols .. ','):gmatch('%s*([^,]+),') do t.columns[#t.columns + 1] = def:match('^([%w_]+)') end
      db.tables[cname] = t
      return {changes = 0, id = 0}
    end
    local iname = sql:match('^CREATE INDEX ([%w_]+) ON ')
    if iname then
      db.indexes = db.indexes or {}
      if db.indexes[iname] then return err('sql_error', 'SQLITE_ERROR: index ' .. iname .. ' already exists') end
      db.indexes[iname] = true
      return {changes = 0, id = 0}
    end
    -- ALTER TABLE t ADD COLUMN c ... [DEFAULT '']: the existing rows get the default (or NULL).
    local atab, acol, adef = sql:match("^ALTER TABLE ([%w_]+) ADD COLUMN ([%w_]+)(.*)$")
    if atab then
      local t = db.tables[atab]
      if not t then return err('sql_error', 'SQLITE_ERROR: no such table: ' .. atab) end
      for _, c in ipairs(t.columns) do if c == acol then return err('sql_error', 'SQLITE_ERROR: duplicate column name: ' .. acol) end end
      t.columns[#t.columns + 1] = acol
      if adef:match("DEFAULT ''") then for _, row in ipairs(t.rows) do row[acol] = '' end end
      return {changes = 0, id = 0}
    end
    local tname, icols = sql:match('^INSERT INTO ([%w_]+) %(([^)]*)%) VALUES')
    if tname then
      local t = db.tables[tname]
      if not t then return err('sql_error', 'SQLITE_ERROR: no such table: ' .. tname) end
      t.seq = t.seq + 1
      local row = {id = t.seq, done = 0}
      for c in icols:gmatch('[%w_]+') do row[c] = nextp() end
      -- n INTEGER PRIMARY KEY: SQLite gives the row number when the insert names no n.
      for _, c in ipairs(t.columns) do if c == 'n' and row.n == nil then row.n = t.seq end end
      if row.text == nil and tname == 'todos' then return err('sql_error', 'SQLITE_CONSTRAINT_NOTNULL: NOT NULL constraint failed: todos.text') end
      t.rows[#t.rows + 1] = row
      return {changes = 1, id = t.seq}
    end
    -- sqlite_master: one row (name) for each table. A test adds a hub table (lazyrex_ops) to tables.
    if sql:match('^SELECT name FROM sqlite_master ') then
      local rows = {}
      for tn in pairs(db.tables) do rows[#rows + 1] = {tn} end
      for xn in pairs(db.indexes or {}) do rows[#rows + 1] = {xn} end
      table.sort(rows, function(a, b) return a[1] < b[1] end)
      return {columns = {'name'}, rows = rows, complete = true}
    end
    -- SELECT cols FROM t [WHERE c op ? AND ...] [GROUP BY c] [ORDER BY c [DESC], ...] [LIMIT n].
    -- A column is a name, or COUNT(*), MIN(c) or MAX(c) with AS name. op is =, < or >.
    local sel, from, rest = sql:match('^SELECT (.-) FROM ([%w_]+)%s*(.-)$')
    if sel then
      local t = db.tables[from]
      rest = trim((rest:gsub('%-%-[^\n]*', ''):gsub(';%s*$', '')))
      if not t then return err('sql_error', 'SQLITE_ERROR: no such table: ' .. from) end
      local function clause(word)
        local after = rest:match(word .. ' (.*)$')
        if not after then return nil end
        for _, stop in ipairs({' GROUP BY ', ' ORDER BY ', ' LIMIT '}) do
          local i = after:find(stop, 1, true)
          if i then after = after:sub(1, i - 1) end
        end
        return trim(after)
      end
      local where, group, order, limit = clause('WHERE'), clause('GROUP BY'), clause('ORDER BY'), tonumber(clause('LIMIT'))
      local conds = {}
      if where then
        for term in (where .. ' AND '):gmatch('(.-) AND ') do
          local c, op = term:match('^([%w_]+) ([=<>]) %?$')
          if c then conds[#conds + 1] = {col = c, op = op, want = nextp()} end
        end
      end
      local function less(a, b) if a == nil then return b ~= nil end if b == nil then return false end return a < b end
      local hits = {}
      for _, row in ipairs(t.rows) do
        local ok = true
        for _, c in ipairs(conds) do
          local v = row[c.col]
          if c.op == '=' then ok = ok and v == c.want
          elseif c.op == '<' then ok = ok and v ~= nil and less(v, c.want)
          else ok = ok and v ~= nil and less(c.want, v) end
        end
        if ok then hits[#hits + 1] = row end
      end
      local cols = {}
      if sel == '*' then for _, c in ipairs(t.columns) do cols[#cols + 1] = {name = c, col = c} end
      else
        for item in (sel .. ','):gmatch('%s*([^,]+),') do
          item = trim(item)
          local fn, arg, as = item:match('^(%a+)%(([%w_*]+)%) AS ([%w_]+)$')
          if fn then cols[#cols + 1] = {name = as, fn = fn:upper(), col = arg} else cols[#cols + 1] = {name = item, col = item} end
        end
      end
      local aggregate = false
      for _, c in ipairs(cols) do if c.fn then aggregate = true end end
      local recs = {}
      if aggregate or group then
        local groups, order_of = {}, {}
        for _, row in ipairs(hits) do
          local g = group and row[group] or ''
          if not groups[g] then groups[g] = {}; order_of[#order_of + 1] = g end
          table.insert(groups[g], row)
        end
        if not group and #order_of == 0 then order_of[1] = ''; groups[''] = {} end
        for _, g in ipairs(order_of) do
          local rec = {}
          for _, c in ipairs(cols) do
            if c.fn == 'COUNT' then rec[c.name] = #groups[g]
            elseif c.fn == 'MIN' or c.fn == 'MAX' then
              local best
              for _, r in ipairs(groups[g]) do
                local v = r[c.col]
                if v ~= nil and (best == nil or (c.fn == 'MIN' and less(v, best)) or (c.fn == 'MAX' and less(best, v))) then best = v end
              end
              rec[c.name] = best
            else rec[c.name] = groups[g][1] and groups[g][1][c.col] end
          end
          recs[#recs + 1] = rec
        end
      else
        for _, row in ipairs(hits) do
          local rec = {}
          for k, v in pairs(row) do rec[k] = v end
          recs[#recs + 1] = rec
        end
      end
      if order then
        local keys = {}
        for term in (order .. ','):gmatch('%s*([^,]+),') do
          local c, desc = trim(term):match('^([%w_]+)%s*(%a*)$')
          keys[#keys + 1] = {col = c, desc = desc:upper() == 'DESC'}
        end
        table.sort(recs, function(a, b)
          for _, k in ipairs(keys) do
            local x, y = a[k.col], b[k.col]
            if x ~= y then if k.desc then return less(y, x) end return less(x, y) end
          end
          return false
        end)
      end
      local names, out = {}, {}
      for i, c in ipairs(cols) do names[i] = c.name end
      for i, rec in ipairs(recs) do
        if limit and i > limit then break end
        local r = {}
        for k, c in ipairs(cols) do r[k] = wire(rec[c.name]) end
        out[#out + 1] = r
      end
      return {columns = names, rows = out, complete = true}
    end

    local utab, set, ucol = sql:match('^UPDATE ([%w_]+) SET (.-) WHERE ([%w_]+) = %?$')
    if utab then
      local t = db.tables[utab]
      if not t then return err('sql_error', 'SQLITE_ERROR: no such table: ' .. utab) end
      -- SET col = expr, or a list of col = ? (the SDK's own tables).
      local sets = {}
      for col, expr in (set .. ','):gmatch('%s*([%w_]+) = ([^,]+),') do sets[#sets + 1] = {col = col, expr = trim(expr)} end
      -- An expr can also be a || list of ?, 'text', a column and length(CAST(col AS BLOB)).
      for _, s in ipairs(sets) do
        if s.expr == '?' then s.p = nextp()
        elseif s.expr:find('||', 1, true) then
          s.cat = {}
          for item in (s.expr .. ' || '):gmatch('(.-) || ') do
            item = trim(item)
            local lit, len = item:match("^'(.*)'$"), item:match('^length%(CAST%(([%w_]+) AS BLOB%)%)$')
            if item == '?' then s.cat[#s.cat + 1] = {v = nextp()}
            elseif lit then s.cat[#s.cat + 1] = {v = lit}
            elseif len then s.cat[#s.cat + 1] = {len = len}
            else s.cat[#s.cat + 1] = {col = item} end
          end
        end
      end
      local want = nextp()
      local changes = 0
      for _, row in ipairs(t.rows) do
        if row[ucol] == want then
          changes = changes + 1
          -- Every expr reads the row before the update, as in SQL.
          local new = {}
          for i, s in ipairs(sets) do
            local col, expr = s.col, s.expr
            if s.cat then
              local out = {}
              for _, c in ipairs(s.cat) do out[#out + 1] = c.v or (c.len and #row[c.len]) or row[c.col] end
              new[i] = table.concat(out)
            elseif expr == '1 - ' .. col then new[i] = 1 - row[col] elseif expr == '?' then new[i] = s.p else new[i] = tonumber(expr) or expr end
          end
          for i, s in ipairs(sets) do row[s.col] = new[i] end
        end
      end
      return {changes = changes, id = 0}
    end
    -- DELETE ... WHERE col = ? or col < ? (a NULL is never less: SQLite).
    local dtab, dcol, dop = sql:match('^DELETE FROM ([%w_]+) WHERE ([%w_]+) ([=<]) %?$')
    if dtab then
      local t = db.tables[dtab]
      local want, keep, changes = nextp(), {}, 0
      for _, row in ipairs(t.rows) do
        local hit = (dop == '=' and row[dcol] == want) or (dop == '<' and row[dcol] ~= nil and row[dcol] < want)
        if hit then changes = changes + 1 else keep[#keep + 1] = row end
      end
      t.rows = keep
      return {changes = changes, id = 0}
    end
    return err('sql_error', 'fake_sql does not know: ' .. sql)
  end

  local handlers = {}
  function handlers.begin(args)
    if fake.open then return err('busy', 'A transaction is open') end
    fake.next_token = fake.next_token + 1
    fake.open = {token = 'tx' .. fake.next_token, db = copy(fake.state), ops = {}}
    return {tx = fake.open.token}
  end
  local function in_tx(args)
    if args.tx == nil then return nil end
    if not fake.open or fake.open.token ~= args.tx then return false end
    return fake.open
  end
  local function statement(args, write)
    local t = in_tx(args)
    if t == false then return err('tx_expired', 'The transaction expired') end
    if t then return run(t.db, args.sql, args.params) end
    if write then
      local db = copy(fake.state)
      local r, e = run(db, args.sql, args.params)
      if r then fake.state = db end
      return r, e
    end
    return run(copy(fake.state), args.sql, args.params)
  end
  function handlers.exec(args) return statement(args, true) end
  function handlers.query(args)
    if not trim(args.sql):upper():match('^SELECT') and not trim(args.sql):upper():match('^PRAGMA USER_VERSION$') then return err('refused', 'A query cannot write') end
    local r, e = statement(args, false)
    -- max_rows: a reply holds at most this many rows (the hub stops at about 1 MB: complete = false).
    if r and fake.max_rows and r.rows and #r.rows > fake.max_rows then
      for i = #r.rows, fake.max_rows + 1, -1 do r.rows[i] = nil end
      r.complete = false
    end
    return r, e
  end
  function handlers.batch(args)
    local t = in_tx(args)
    if t == false then return err('tx_expired', 'The transaction expired') end
    local db = t and t.db or copy(fake.state)
    local work = copy(db)
    local results = {}
    for i, s in ipairs(args.statements or {}) do
      local r, e = run(work, s.sql, s.params)
      if not r then return nil, e end
      results[i] = r
    end
    if t then t.db = work else fake.state = work end
    return {results = results}
  end
  function handlers.commit(args)
    local t = in_tx(args)
    if not t then return err('tx_expired', 'The transaction expired') end
    if args.key then
      local saved = fake.ops[args.key]
      if saved then
        fake.open = nil
        if saved.fingerprint ~= args.fingerprint then return err('operation_conflict', 'This key has different content') end
        return {replayed = true, reply = saved.reply}
      end
    end
    fake.state = t.db
    if args.key then fake.ops[args.key] = {fingerprint = args.fingerprint, reply = args.reply_parts and fake.resolve_parts(args.reply_parts) or copy(args.reply)} end
    fake.notices = fake.notices or {}
    for _, n in ipairs(args.notices or {}) do fake.notices[#fake.notices + 1] = n end
    fake.open = nil
    fake.commits = (fake.commits or 0) + 1
    return {}
  end
  function handlers.rollback(args)
    if fake.open and fake.open.token == args.tx then fake.open = nil end
    fake.rollbacks = (fake.rollbacks or 0) + 1
    return {}
  end
  function handlers.replay(args)
    local saved = fake.ops[args.key]
    if not saved then return {found = false} end
    if saved.fingerprint ~= args.fingerprint then return err('operation_conflict', 'This key has different content') end
    return {found = true, reply = copy(saved.reply)}
  end

  function fake.handle(action, args)
    fake.calls[#fake.calls + 1] = {action = action, args = copy(args)}
    local hook = fake.hooks[action]
    if hook then hook(args) end
    local f = fake.fail_next[action]
    if f then fake.fail_next[action] = nil; return nil, f end
    local name = action:match('^hub%.sql%.(%w+)$')
    local h = name and handlers[name]
    if not h then return err('not_supported', 'fake_sql: ' .. action) end
    local r, e = h(args or {})
    local lost = fake.lose_reply[action]
    if lost and r then fake.lose_reply[action] = nil; return nil, lost end
    return r, e
  end
  -- count answers the number of logged calls of action.
  function fake.count(action)
    local n = 0
    for _, c in ipairs(fake.calls) do if c.action == action then n = n + 1 end end
    return n
  end
  function fake.rows(name) return (fake.state.tables[name] or {rows = {}}).rows end
  return fake
end

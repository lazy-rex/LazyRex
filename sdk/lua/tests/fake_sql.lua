-- A fake hub.sql for the SDK tests (wire: hub.sql handoff v1 and the NULL tag {null = true}). It
-- keeps tables in memory, and it knows only the statements that the tests use: CREATE TABLE,
-- PRAGMA user_version, and INSERT, SELECT, UPDATE and DELETE of the forms below. A handler
-- transaction buffers its changes on a copy; commit installs the copy, rollback drops it. Saved
-- replies commit with the transaction. Every call is logged in fake.calls.
--   local fake = dofile('fake_sql.lua')()
--   fake.handle(action, args) -> result | nil, {code, message}
-- fake.fail_next[action] = {code, message} fails the next call of that action once.
-- fake.on(action, fn(args)) runs fn before the call (for example to make a commit fail).
return function()
  local fake = {calls = {}, ops = {}, state = {tables = {}, user_version = 0}, open = nil, next_token = 0, fail_next = {}, hooks = {}}
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
    local cname, cols = sql:match('^CREATE TABLE (%w+) %((.*)%)$')
    if cname then
      if db.tables[cname] then return err('sql_error', 'SQLITE_ERROR: table ' .. cname .. ' already exists') end
      local t = {columns = {}, rows = {}, seq = 0}
      for def in (cols .. ','):gmatch('%s*([^,]+),') do t.columns[#t.columns + 1] = def:match('^(%w+)') end
      db.tables[cname] = t
      return {changes = 0, id = 0}
    end
    local tname, icols = sql:match('^INSERT INTO (%w+) %(([^)]*)%) VALUES')
    if tname then
      local t = db.tables[tname]
      if not t then return err('sql_error', 'SQLITE_ERROR: no such table: ' .. tname) end
      t.seq = t.seq + 1
      local row = {id = t.seq, done = 0}
      for c in icols:gmatch('%w+') do row[c] = nextp() end
      if row.text == nil and tname == 'todos' then return err('sql_error', 'SQLITE_CONSTRAINT_NOTNULL: NOT NULL constraint failed: todos.text') end
      t.rows[#t.rows + 1] = row
      return {changes = 1, id = t.seq}
    end
    local sel, from = sql:match('^SELECT (.-) FROM (%w+)')
    if sel then
      local t = db.tables[from]
      if not t then return err('sql_error', 'SQLITE_ERROR: no such table: ' .. from) end
      local names = {}
      if sel == '*' then names = copy(t.columns) else for c in sel:gmatch('[^,%s]+') do names[#names + 1] = c end end
      local where = sql:match('WHERE (%w+) = %?')
      local want = where and nextp()
      local out = {}
      for _, row in ipairs(t.rows) do
        if not where or row[where] == want then
          local r = {}
          for k, c in ipairs(names) do r[k] = wire(row[c]) end
          out[#out + 1] = r
        end
      end
      return {columns = names, rows = out, complete = true}
    end
    local utab, set, ucol = sql:match('^UPDATE (%w+) SET (.-) WHERE (%w+) = %?$')
    if utab then
      local t = db.tables[utab]
      if not t then return err('sql_error', 'SQLITE_ERROR: no such table: ' .. utab) end
      local col, expr = set:match('^(%w+) = (.*)$')
      local p = expr == '?' and nextp() or nil
      local want = nextp()
      local changes = 0
      for _, row in ipairs(t.rows) do
        if row[ucol] == want then
          changes = changes + 1
          if expr == '1 - ' .. col then row[col] = 1 - row[col] elseif expr == '?' then row[col] = p else row[col] = tonumber(expr) or expr end
        end
      end
      return {changes = changes, id = 0}
    end
    local dtab, dcol = sql:match('^DELETE FROM (%w+) WHERE (%w+) = %?$')
    if dtab then
      local t = db.tables[dtab]
      local want, keep, changes = nextp(), {}, 0
      for _, row in ipairs(t.rows) do if row[dcol] == want then changes = changes + 1 else keep[#keep + 1] = row end end
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
    return statement(args, false)
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
    if args.key then fake.ops[args.key] = {fingerprint = args.fingerprint, reply = copy(args.reply)} end
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
    return h(args or {})
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

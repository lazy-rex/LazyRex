-- A base step on the real hub SQL. The plugin database already has the hub's own tables
-- (lazyrex_ops, lazyrex_notice_outbox) when the plugin first starts, and it still counts as new.

local alice = agent('alice')
-- exits counts the exits with code 1 of plugin sm in the lab hub log (no test helper reports them).
local function exits()
  local f = io.open(H.args.lab .. '/hub.log', 'r')
  if not f then return 0 end
  local text, n = f:read('*a'), 0
  f:close()
  for _ in text:gmatch('plugin sm exited: {"exit_code":1') do n = n + 1 end
  return n
end

test('a new database runs the base and the later step: version 4, the tables a and b, beside the hub tables', function()
  local r = alice:call('sm.tables', {})
  expect(r.version).to_be(4)
  local mine, hub = {}, 0
  for _, n in ipairs(r.tables) do
    if n:sub(1, 8) == 'lazyrex_' then hub = hub + 1 elseif n:sub(1, 7) ~= 'sqlite_' then mine[#mine + 1] = n end
  end
  expect(mine).to_equal({'a', 'b'})
  assert_true(hub > 0, 'the hub tables are there')
end)

test('a database below the base refuses to start, and nothing changes', function()
  sql_write('DROP TABLE b')
  sql_write('PRAGMA user_version = 1')
  local before = exits()
  pcall(H.hub, 'hub.admin.stop', {plugin = 'sm'})
  pcall(H.hub, 'hub.admin.start', {plugin = 'sm'})
  -- The refusal ends the start with exit 1 (the hub then respawns it with a backoff).
  wait('the start refuses', function() return exits() > before end, 15)
  expect(sql('PRAGMA user_version')[1].user_version, 'the version stays').to_be(1)
  expect(sql("SELECT name FROM sqlite_master WHERE name = 'b'")[1], 'no step ran').to_be(nil)
end)

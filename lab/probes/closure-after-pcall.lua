-- Probe (rex-protocol.md, "Rex Lua runtime"): a closure write to a captured local after a caught error.
-- Run: rex -S <lab> --autostart=false do <this file> --all
local function case_minimal()
  local n = 0
  local inc = function() n = n + 1 end
  pcall(error, 'x')
  inc()
  return n
end
local function case_no_catch()
  local n = 0
  local inc = function() n = n + 1 end
  pcall(function() end)
  inc()
  return n
end
local function case_table()
  local box = {n = 0}
  local inc = function() box.n = box.n + 1 end
  pcall(error, 'x')
  inc()
  return box.n
end
local function case_deeper()
  local n = 0
  local inc = function() n = n + 1 end
  local function deep() pcall(error, 'x') end
  deep()
  inc()
  return n
end
print('PROBE minimal=' .. case_minimal() .. ' no_catch=' .. case_no_catch() .. ' table=' .. case_table() .. ' deeper=' .. case_deeper() .. ' (standard Lua: 1 1 1 1)')

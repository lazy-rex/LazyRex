-- The test runner that `lazyrex-hub test` starts with `rex do` for each test file
-- (docs/testing.md). It prints only marked result lines on stdout:
--   LRT plan <n>            the number of tests that will run
--   LRT ok <i> <name>       a test passed
--   LRT fail <i> <name> # <message at file:line>
--   LRT note <text>         information (for example the parity report)
--   LRT done <passed> <failed>
-- The harness fails the file without a plan or a done line, so an early exit cannot pass.

local A = rex.args
package.path = A.lib .. '/?.lua;' .. package.path
local H = require('harness')

local function out(line)
  io.write('LRT ', (line:gsub('\\', '\\\\'):gsub('\n', '\\n')), '\n')
  io.flush()
end

local tests = {}
local env = setmetatable({}, {__index = _G})
for _, name in ipairs({'expect', 'expect_error', 'assert_eq', 'assert_true', 'store', 'notice', 'batches', 'advance_rev',
  'tabs', 'tab', 'views', 'operation', 'inject', 'injection', 'with_injection', 'session', 'close_session', 'write', 'put_operation', 'write_profiles', 'invalidates', 'fault', 'fault_fired', 'fault_release', 'agent', 'user', 'node', 'wait', 'show', 'equal', 'is_error'}) do
  env[name] = H[name]
end
env.H = H
env.test = function(name, fn)
  if type(name) ~= 'string' or type(fn) ~= 'function' then error('test(name, function) expected', 2) end
  for _, t in ipairs(tests) do
    if t.name == name then error('test ' .. string.format('%q', name) .. ' is declared twice', 2) end
  end
  tests[#tests + 1] = {name = name, fn = fn}
end

-- where finds the first stack frame in the test file, for the failure location.
local function where(file)
  for level = 3, 40 do
    local info = debug.getinfo(level, 'Sl')
    if not info then break end
    if info.source == '@' .. file and info.currentline and info.currentline > 0 then
      return file:match('[^/]*$') .. ':' .. info.currentline
    end
  end
  return nil
end

local function load_file(path)
  local chunk, err = loadfile(path)
  if not chunk then return nil, err end
  setfenv(chunk, env)
  return chunk
end

local setup_ok, setup_err = pcall(H.setup, A)
local chunk, load_err
if setup_ok then chunk, load_err = load_file(A.file) end
if not setup_ok or not chunk then
  out('plan 1')
  out('fail 1 setup # ' .. tostring(setup_ok and load_err or setup_err))
  out('done 0 1')
  return
end
env.FIXTURE = A.fixture
env.PARITY_ROUTES = A.parity_routes and rex.json.decode(A.parity_routes) or {}
env.load_fixture = function(path)
  local f, err = load_file(path)
  if not f then error(err, 2) end
  return f()
end
local ok, err = pcall(chunk)
if not ok then
  out('plan 1')
  out('fail 1 load # ' .. tostring(err))
  out('done 0 1')
  return
end

local selected = {}
for _, t in ipairs(tests) do
  if not A.run or A.run == '' or t.name:find(A.run) then selected[#selected + 1] = t end
end
if #selected == 0 then
  out('plan 0')
  out('done 0 0')
  return
end
-- One more test closes every file: injections that a test set up must have been consumed, and
-- every rendered tree must pass the parity check.
out('plan ' .. (#selected + 1))
local passed, failed = 0, 0
for i, t in ipairs(selected) do
  local loc
  local okt, e = xpcall(t.fn, function(e)
    loc = where(A.file)
    if H.is_error(e) then return tostring(e) .. (e.raw and e.raw ~= tostring(e) and (' [' .. e.raw .. ']') or '') end
    return tostring(e)
  end)
  if okt then
    passed = passed + 1
    out('ok ' .. i .. ' ' .. t.name)
  else
    failed = failed + 1
    out('fail ' .. i .. ' ' .. t.name .. ' # ' .. tostring(e) .. (loc and (' (at ' .. loc .. ')') or ''))
  end
end

local last = #selected + 1
local okf, fin = pcall(H.hub, 'hub.test.finish', {})
local problems = {}
if not okf then
  problems[1] = 'hub.test.finish failed: ' .. tostring(fin)
else
  for _, u in ipairs(fin.unconsumed or {}) do
    problems[#problems + 1] = string.format('injection %d (plugin %s) consumed %d of %d', u.id, u.plugin, u.consumed, u.times)
  end
  for _, f in ipairs(fin.unfired or {}) do
    local what = f.fired and 'was not released' or 'did not fire'
    problems[#problems + 1] = string.format('fault %d (%s %s %s %s) %s', f.id, f.point, f.mode, f.action or '', f.when or '', what)
  end
  for _, v in ipairs((fin.parity or {}).violations or {}) do
    if v.plugin == A.plugin then
      problems[#problems + 1] = string.format('parity: %s %s %s: %s', v.route or ('command ' .. tostring(v.command)),
        v.node or '', v.action or '', v.problem)
    end
  end
end
if #problems == 0 then
  passed = passed + 1
  out('ok ' .. last .. ' file checks (injections consumed, faults fired, parity of rendered trees)')
else
  failed = failed + 1
  out('fail ' .. last .. ' file checks # ' .. table.concat(problems, '\n'))
end
out('done ' .. passed .. ' ' .. failed)

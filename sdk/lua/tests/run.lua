-- The SDK test runner. It needs no hub: run it with rex do on a lab Rex server.
--   rex -S unix:///tmp/rexlab-<team>-<you>/rex.sock --autostart=false do <sdk dir>/tests/run.lua [pattern=<Lua pattern>]
-- It runs every test of the files in FILES, prints one line for each, and exits with 1 when one fails.
local tests_dir = debug.getinfo(1, 'S').source:match('^@?(/.*)/[^/]*$')
local sdk_dir = tests_dir:match('^(.*)/tests$')
local FILES = {'sdk_test.lua'}
local pattern = rex.args and rex.args.pattern

local list = {}
local env = setmetatable({}, {__index = _G})
env.sdk_dir, env.tests_dir = sdk_dir, tests_dir
function env.test(name, fn) list[#list + 1] = {name = name, fn = fn} end

-- show answers a short text for a value.
local function show(v, depth)
  depth = depth or 0
  if type(v) == 'string' then return string.format('%q', v) end
  if type(v) ~= 'table' then return tostring(v) end
  if depth > 4 then return '{...}' end
  local keys = {}
  for k in pairs(v) do keys[#keys + 1] = k end
  table.sort(keys, function(a, b) return tostring(a) < tostring(b) end)
  local parts = {}
  for _, k in ipairs(keys) do parts[#parts + 1] = tostring(k) .. '=' .. show(v[k], depth + 1) end
  return '{' .. table.concat(parts, ', ') .. '}'
end
env.show = show
local function same(a, b)
  if type(a) ~= type(b) then return false end
  if type(a) ~= 'table' then return a == b end
  for k, v in pairs(a) do if not same(v, b[k]) then return false end end
  for k in pairs(b) do if a[k] == nil then return false end end
  return true
end
-- expect(value, what).to_be(x) / .to_equal(t) / .to_match(pattern)
function env.expect(v, what)
  local label = what and (what .. ': ') or ''
  return {
    to_be = function(x) if v ~= x then error(label .. 'expected ' .. show(x) .. ', got ' .. show(v), 2) end end,
    to_equal = function(x) if not same(v, x) then error(label .. 'expected ' .. show(x) .. ', got ' .. show(v), 2) end end,
    to_match = function(p) if type(v) ~= 'string' or not v:match(p) then error(label .. 'expected a match of ' .. show(p) .. ', got ' .. show(v), 2) end end,
  }
end

for _, f in ipairs(FILES) do
  local chunk = assert(loadfile(tests_dir .. '/' .. f))
  setfenv(chunk, env)
  chunk()
end
local passed, failed, ran = 0, 0, 0
for _, t in ipairs(list) do
  if not pattern or t.name:match(pattern) then
    ran = ran + 1
    local ok, err = pcall(t.fn)
    if ok then passed = passed + 1; print('ok    ' .. t.name)
    else failed = failed + 1; print('FAIL  ' .. t.name .. '\n      ' .. tostring(err)) end
  end
end
print(string.format('%d passed, %d failed', passed, failed))
if failed > 0 or ran == 0 then os.exit(1) end

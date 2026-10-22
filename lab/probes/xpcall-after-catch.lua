-- Probe (rex-protocol.md, "Rex Lua runtime"): which catches split a captured local, and what an
-- xpcall frame changes. Each case returns 2 in standard Lua 5.1, except xpcall_passes_args: Lua 5.1
-- xpcall takes only f and a handler (extra arguments came in Lua 5.2), so it returns 1.
-- Run: rex -S <lab> --autostart=false do <this file>
local h = function(e) return e end
-- w makes local n and closure f. f adds 1 and raises on its first call. pre runs first.
local function w(pre)
  local n = 0
  local function f() n = n + 1; if n == 1 then error('x') end end
  pre()
  pcall(f)
  f()
  return n
end
local function none() end
local function under_xpcall(pre) return select(2, xpcall(function() return w(pre) end, h)) end
local function go_error() local n = 0; local f = function() n = n + 1 end; f(); pcall(rex.call, 'no.such.method', {}); f(); return n end
local function args() local got; xpcall(function(a) got = a end, h, 'a'); return got == 'a' and 2 or 1 end
local cases = {
  {'bare', function() return w(none) end},
  {'under_pcall', function() return select(2, pcall(w, none)) end},
  {'under_xpcall', function() return under_xpcall(none) end},
  {'under_xpcall_after_pcall_ok', function() return under_xpcall(function() pcall(tostring, 1) end) end},
  {'under_xpcall_after_xpcall_ok', function() return under_xpcall(function() xpcall(tostring, h) end) end},
  {'under_xpcall_in_coroutine', function() return select(2, xpcall(function() return select(2, coroutine.resume(coroutine.create(function() return w(none) end))) end, h)) end},
  {'go_side_error', go_error},
  {'xpcall_passes_args', args},
}
local out = {}
for _, c in ipairs(cases) do out[#out + 1] = c[1] .. '=' .. tostring(c[2]()) end
print('PROBE ' .. table.concat(out, ' ') .. ' (standard Lua 5.1: all 2, xpcall_passes_args 1)')

-- Probe (rex-protocol.md, "Rex Lua runtime"): run this file with rex do from a read-only
-- directory with a hash name, with lib/sib.lua (return 'sibling-loaded') and data.txt beside it.
local dir = assert(debug.getinfo(1, 'S').source:match('^@?(/.*)/[^/]*$'), 'no dir')
local sib = dofile(dir .. '/lib/sib.lua')
local f = io.open(dir .. '/data.txt', 'r'); local data = f and f:read('*a') or 'nil'; if f then f:close() end
local w, werr = io.open(dir .. '/write-test', 'w')
print('PROBE dir=' .. dir .. ' sib=' .. tostring(sib) .. ' data=' .. data .. ' write=' .. tostring(w ~= nil) .. ' ' .. tostring(werr))
print('PROBE package.path=' .. tostring(package and package.path))
local ok, mod = pcall(require, 'lib.sib')
print('PROBE require lib.sib ok=' .. tostring(ok) .. ' ' .. tostring(mod))

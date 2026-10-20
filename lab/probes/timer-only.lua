-- A script with a timer and no rex.on handler: does it keep running?
local S = {n = 0}
S.t = rex.every(1, function() S.n = S.n + 1; io.stdout:write('tick ', S.n, '\n'); io.stdout:flush() end)

-- Test helpers for a plugin test that runs the SDK in its own sandbox (docs/testing.md). Production
-- code never loads this file: lazyrex.lua does not require it. A plugin test loads it with
--   local testing = dofile(H.args.sdk .. '/testing.lua')
local M = {}

-- LIMITS are TEST values for the limits of the hub.register reply, not the hub's: the hub sends its
-- own (hub/cmd/hub/replyparts.go, partsLimits), and they are the one source for a real plugin. The
-- values are small, so a sandbox test reaches the reply parts with small replies.
M.LIMITS = {inline_max = 4096, part_bytes = 8192, max_total = 65536}

-- register_reply answers a hub.register reply for a sandbox: generation '1' and a copy of LIMITS,
-- with the optional table fields merged over it (for example {generation = '7'}).
function M.register_reply(fields)
  local r = {generation = '1', limits = {}}
  for k, v in pairs(M.LIMITS) do r.limits[k] = v end
  for k, v in pairs(fields or {}) do r[k] = v end
  return r
end

return M

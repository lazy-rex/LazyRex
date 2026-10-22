-- Times the SDK's own encode and slice steps for the 150 KiB timing payload (view/chunk_lab_test.go
-- TestReplyPartsTimingLab, the sp panel Big). It loads sdk/lua/lazyrex.lua and finds the local
-- functions wire_nodes and slices through upvalues (from the module and an app): the probe runs the
-- SDK's functions, not copies. It does not measure the live path (no handler, no transport, no
-- hub join). Run on a lab server: rex do lab/probes/parts-sdk-timing.lua sdk=<sdk/lua dir> [runs=21]
-- The app that the probe makes installs an event handler, so after the result the script keeps
-- listening; stop it with Ctrl-C.
local dir = assert(rex.args.sdk, 'sdk=<the sdk/lua directory> is required')
package.path = dir .. '/?.lua;' .. package.path
local M = require('lazyrex')

-- wire_nodes is local to lazyrex.plugin, so the probe makes an app. The hub and instance IDs are
-- placeholders, and the probe calls no hub action.
rex.args = rex.args or {}
rex.args.hub_client_id, rex.args.instance_id = 'client:00000000-0000-4000-8000-000000000000', 'probe'
local app = M.plugin('probe')
M._state.register = function() end -- the start timer registers with no hub; the probe needs none

-- find returns the first function upvalue named name. It searches from the functions of M and app,
-- through function upvalues and the tables that they reach.
local function find(name)
  local seen, queue = {}, {M, app}
  while #queue > 0 do
    local v = table.remove(queue, 1)
    if not seen[v] then
      seen[v] = true
      if type(v) == 'table' then
        for _, x in pairs(v) do
          if type(x) == 'function' or type(x) == 'table' then queue[#queue + 1] = x end
        end
      else
        for i = 1, 255 do
          local n, x = debug.getupvalue(v, i)
          if not n then break end
          if n == name and type(x) == 'function' then return x end
          if type(x) == 'function' or type(x) == 'table' then queue[#queue + 1] = x end
        end
      end
    end
  end
  error('no SDK function ' .. name)
end
local slices = find('slices')
local limits = {inline_max = 45056, part_bytes = 57344, max_total = 786432}

-- The payload of the sp panel Big (tests/sdk-parts/main.lua filler(150)) as the SDK replies it.
local text = string.rep('aé"\\<&日', 60)
local function node(id, t) return {id = id, type = 'widget', text = t} end
local per = #rex.json.encode(node('n1000', text)) + 1
local nodes = {node('ticks', 'ticks 1')}
for k = 1, math.floor(150 * 1024 / per) do nodes[#nodes + 1] = node('n' .. k, text) end
-- The SDK's panel reply: tree() passes the nodes through wire_nodes, and a tracked render has no
-- refresh field (lazyrex.lua view_call).
local reply = {tree = {title = 'Big', nodes = find('wire_nodes')(nodes)}}

local runs = tonumber(rex.args.runs) or 21
local enc, sl, bytes, count = {}, {}, 0, 0
for r = 1, runs do
  local t0 = os.clock()
  local encoded = rex.json.encode(reply)
  local t1 = os.clock()
  local parts = slices(encoded, 'probe-' .. r, limits)
  local t2 = os.clock()
  enc[r], sl[r], bytes, count = (t1 - t0) * 1000, (t2 - t1) * 1000, #encoded, #parts
end
table.sort(enc)
table.sort(sl)
local mid = math.floor(runs / 2) + 1
return {encoded_bytes = bytes, parts = count, runs = runs, cpu_ms = {encode_median = enc[mid], slice_median = sl[mid],
  encode_max = enc[runs], slice_max = sl[runs]}}

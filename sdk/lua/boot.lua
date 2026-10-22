-- The plugin launcher of the LazyRex Plugin SDK (coordinator proposal, 2026-10-22). The hub runs
--   rex do <sdk dir>/boot.lua main=<absolute plugin main file> <other args>
-- boot.lua puts the plugin directory and then the SDK directory first in package.path, and runs
-- the main file. So require('lazyrex') and require of a sibling file need no setup in the plugin.
-- rex.args stays as it is. In the main file, debug.getinfo(1, 'S').source is the main file path.
local source = debug.getinfo(1, 'S').source
local sdk_dir = assert(source:match('^@?(/.*)/[^/]*$'), 'The SDK path is unknown')
local main = rex.args and rex.args.main
assert(type(main) == 'string' and main:sub(1, 1) == '/', 'main=<absolute path of the plugin main file> is required')
local plugin_dir = assert(main:match('^(/.*)/[^/]*$'), 'The plugin path is unknown')
package.path = plugin_dir .. '/?.lua;' .. plugin_dir .. '/?/init.lua;' .. sdk_dir .. '/?.lua;' .. package.path
rex.args.lazyrex_sdk = rex.args.lazyrex_sdk or sdk_dir
-- Rex Lua fails table.concat of 2560 items or more; the SDK concat has no limit (lazyrex.lua, "concat").
table.concat = require('lazyrex').concat
dofile(main)

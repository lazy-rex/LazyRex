-- The isolation probe (../probe.lua) as the dependency isodep: a manifest's main file must be
-- inside its own directory (docs/plugin-sources.md, section 2).
local dir = assert(debug.getinfo(1, 'S').source:match('^@?(/.*)/[^/]*$'), 'the probe path is unknown')
dofile(dir .. '/../probe.lua')

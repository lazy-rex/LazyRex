-- The harness fixture (tests/harness/fixture.lua), loaded from this directory: a manifest's main
-- file must be inside its own directory (docs/plugin-sources.md, section 2).
local dir = assert(debug.getinfo(1, 'S').source:match('^@?(/.*)/[^/]*$'), 'the fixture path is unknown')
dofile(dir .. '/../harness/fixture.lua')

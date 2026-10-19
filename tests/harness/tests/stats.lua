-- hub.test.stats: per-plugin counts of store batches and hub.ui.invalidate calls.

test('invalidates counts only the hub.ui.invalidate calls of the plugin', function()
  local before = invalidates()
  local other = invalidates('lazyrex')
  rex.sleep(1)
  expect(invalidates(), 'no idle invalidate').to_be(before)
  agent('alice'):call('fix.invalidate', {})
  agent('alice'):call('fix.invalidate', {})
  expect(invalidates()).to_be(before + 2)
  expect(invalidates('lazyrex'), 'another plugin keeps its own count').to_be(other)
end)

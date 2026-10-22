-- This suite has test.agents, so its test agents are in roster mode (docs/testing.md, "Agents"):
-- report and kind are identity mode options, and the harness refuses them before it opens a pane.

test('report is refused in roster mode', function()
  expect_error(function() agent('alice', {report = false}) end, 'are the identity mode')
  expect_error(function() agent('mode-new', {report = true}) end, 'are the identity mode')
  expect(agent('alice').id).to_be('alice')
end)

test('kind is refused in roster mode', function()
  expect_error(function() agent('bob', {kind = 'codex'}) end, 'are the identity mode')
end)

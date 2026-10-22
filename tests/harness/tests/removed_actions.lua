-- App mode is removed (docs/hub.md, "Removed: app mode"). The hub does not offer its actions, so Rex
-- refuses a call before it reaches the hub (observed on Rex 0.0.1792158932-g88fd41).

local alice = agent('alice')

test('hub.ui.exit and hub.ui.mode are not offered', function()
  for _, action in ipairs({'hub.ui.exit', 'hub.ui.mode'}) do
    local r = alice:call('fix.hub', {action = action, args = {}})
    expect(r.error, action .. ' is refused').to_be_truthy()
    expect(r.error.message, action).to_match('does not offer')
  end
end)

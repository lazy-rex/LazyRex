-- want: names no tool (an exported agent action) and no user_only reason
-- A later render without the bare action must not hide the violation of the earlier render.
test('renders a bare action, then a clean tree', function()
  local v = user:open('home')
  agent('alice'):call('fix.put', {key = 'mode', value = 'noparity'})
  v:render()
  agent('alice'):call('fix.put', {key = 'mode', value = 'normal'})
  v:render()
end)

-- want: names no tool (an exported agent action) and no user_only reason
test('renders a tree with a bare action', function()
  agent('alice'):call('fix.put', {key = 'mode', value = 'noparity'})
  user:open('home'):render()
end)

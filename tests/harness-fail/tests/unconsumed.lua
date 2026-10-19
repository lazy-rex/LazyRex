-- want: injection 1 (plugin harnessfix) consumed 0 of 1
test('sets up an injection that never matches', function()
  inject({match = {writes = 'never'}, ops = {{key = 'x', value = 1}}})
end)

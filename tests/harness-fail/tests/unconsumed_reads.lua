-- want: injection 1 (plugin harnessfix) consumed 0 of 1
test('sets up a reads injection that never matches', function()
  inject({match = {reads = 'never'}, ops = {{key = 'x', value = 1}}})
end)

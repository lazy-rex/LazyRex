-- want: runner exited
test('passes', function() end)
test('crashes', function() os.exit(3) end)

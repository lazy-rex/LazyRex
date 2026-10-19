-- want: 2 passed, 1 failed
test('passes', function() expect(1).to_be(1) end)
test('fails', function() expect(1).to_be(2) end)

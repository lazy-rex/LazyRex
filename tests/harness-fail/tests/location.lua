-- want: (at location.lua:3)
test('a failure names its line in the test file', function()
  expect(1).to_be(2)
end)

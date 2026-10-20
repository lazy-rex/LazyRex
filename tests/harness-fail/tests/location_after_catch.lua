-- want: (at location_after_catch.lua:6)
test('a failure after caught errors still names its line', function()
  expect_error(function() error('caught') end, 'caught')
  local n = {0}
  wait('a callback that fails once', function() n[1] = n[1] + 1; if n[1] == 1 then error('once') end; return true end)
  expect(1).to_be(2)
end)

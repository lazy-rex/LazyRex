-- wait() returns the callback's first truthy result, also after the callback raised (docs/testing.md,
-- "Waiting"). Tests read that return value instead of assigning outer locals in the callback.

test('wait returns a table after the callback raised once', function()
  local calls = {n = 0}
  local got = wait('a table after one error', function()
    calls.n = calls.n + 1
    if calls.n == 1 then error('transient') end
    return {value = 'ready', calls = calls.n}
  end)
  expect(got).to_equal({value = 'ready', calls = 2})
end)

test('wait returns the first truthy value, not false or nil', function()
  local seq = {false, nil, 0}
  local i = {n = 0}
  local got = wait('a truthy value', function()
    i.n = i.n + 1
    return seq[i.n]
  end)
  expect(got).to_be(0)
  expect(i.n).to_be(3)
end)

test('wait fails with what and the last error after the timeout', function()
  local e = expect_error(function() wait('never', function() error('still failing') end, 1) end, 'timeout')
  expect(e.message).to_match('timed out waiting for never: .*still failing')
end)

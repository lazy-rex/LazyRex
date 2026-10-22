-- The assertions must fail on a mismatch, in both call styles.
local function fails(fn)
  local ok = pcall(fn)
  if ok then error('expected the assertion to fail', 2) end
end

test('each matcher fails on a mismatch (dot style)', function()
  fails(function() expect(1).to_be(2) end)
  fails(function() expect({a = 1}).to_equal({a = 2}) end)
  fails(function() expect({a = 1}).to_equal({a = 1, b = 2}) end)
  fails(function() expect('abc').to_match('^b') end)
  fails(function() expect({1, 2}).to_contain(3) end)
  fails(function() expect({1, 2}).not_to_contain(2) end)
  fails(function() expect({1, 2}).to_have_length(3) end)
  fails(function() expect(nil).to_be_truthy() end)
  fails(function() expect(1).to_be_falsy() end)
  fails(function() expect(1).not_to_be(1) end)
  fails(function() assert_eq({1}, {2}) end)
  fails(function() assert_true(false) end)
  fails(function() expect_error(function() end) end)
  fails(function() expect_error(function() error('boom') end, 'other') end)
end)

test('each matcher fails on a mismatch (colon style)', function()
  fails(function() expect(1):to_be(2) end)
  fails(function() expect({a = 1}):to_equal({}) end)
  fails(function() expect('abc'):to_match('z') end)
end)

test('each matcher passes on a match', function()
  expect(1).to_be(1)
  expect(1):to_be(1)
  expect({a = {1, 2}}).to_equal({a = {1, 2}})
  expect('abc').to_match('^a')
  expect({1, 2}).to_contain(2)
  expect('hello').to_contain('ell')
  expect({1, 2}).to_have_length(2)
  expect(expect_error(function() error('boom') end, 'boom').message).to_match('boom')
end)

test('a label that is not text keeps the real failure visible', function()
  for _, label in ipairs({{step = 2}, 7, true}) do
    local ok, err = pcall(function() expect(1, label).to_be(2) end)
    expect(ok).to_be(false)
    local text = type(err) == 'table' and tostring(err.message) or tostring(err)
    expect(text).to_match('expected 2, got 1')
    expect(text:find('concatenate', 1, true), 'no concatenation error').to_be(nil)
  end
end)

test('show prints a list of more than 2559 items (Rex Lua table.concat limit)', function()
  local big = {}
  for i = 1, 3000 do big[i] = i end
  local s = H.show(big)
  expect(s:sub(1, 6)).to_be('{1, 2,')
  expect(#s <= 603, 'the text is cut').to_be(true)
end)

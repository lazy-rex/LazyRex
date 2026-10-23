-- The harness's own client checks (the view_of flake, 2026-10-22: a View with an empty client).
-- resolve runs against a fake Rex (H.rex_call); captures go into tables, never into outer locals
-- (docs/testing.md, "Waiting").

-- with_fake runs fn with H.rex_call and H.diag replaced, and returns fn's result and the diag lines.
local function with_fake(answers, fn)
  local real_call, real_diag = H.rex_call, H.diag
  local got = {lines = {}}
  H.rex_call = function(method, payload)
    local a = answers[method]
    if type(a) == 'function' then return a(payload) end
    return a
  end
  H.diag = function(text) got.lines[#got.lines + 1] = text end
  local ok, v = pcall(fn)
  H.rex_call, H.diag = real_call, real_diag
  if not ok then error(v, 0) end
  got.value = v
  return got
end

local function resolved(id) return {results = {{status = 'resolved', target = {client_id = id}}}} end
local function inspect(id) return {client = {client_id = id, connection_state = 'connected'}} end

test('resolve gives a connected client whose ID matches', function()
  local got = with_fake({['target.resolve'] = resolved('client:abc'), ['client.inspect'] = inspect('client:abc')},
    function() return H.resolve('lazyrex-testview:v1') end)
  expect(got.value).to_be('client:abc')
  expect(got.lines).to_have_length(0)
end)

test('resolve refuses an empty client ID and logs both answers', function()
  -- client.inspect {client_id = ""} answers for some connected client (the lead's first hypothesis).
  local got = with_fake({['target.resolve'] = resolved(''), ['client.inspect'] = inspect('client:caller')},
    function() return H.resolve('lazyrex-testview:v1') end)
  expect(got.value).to_be(nil)
  expect(got.lines).to_have_length(1)
  expect(got.lines[1]).to_match('refused candidate ""')
  expect(got.lines[1]).to_match('client:caller')
end)

test('resolve refuses a candidate when client.inspect names another client', function()
  local got = with_fake({['target.resolve'] = resolved('client:abc'), ['client.inspect'] = inspect('client:other')},
    function() return H.resolve('lazyrex-testview:v1') end)
  expect(got.value).to_be(nil)
  expect(got.lines).to_have_length(1)
  expect(got.lines[1]).to_match('client:other')
end)

test('a View never gets a client that is not a client ID', function()
  local v = {id = 'view-x', route = '/forms/x', label = 'lazyrex-testview:view-x'}
  expect(H.check_client(v, 'client:abc')).to_be('client:abc')
  for _, bad in ipairs({'', 'abc', 'client:'}) do
    local e = expect_error(function() H.check_client(v, bad) end, 'bad_client')
    expect(e.message).to_match('view%-x')
  end
  expect_error(function() H.check_client(v, nil) end, 'bad_client')
end)

test('perform refuses a missing client before it calls Rex', function()
  for _, bad in ipairs({'', false}) do
    local e = expect_error(function() H.perform(bad or nil, 'test.action', {}) end, 'bad_client')
    expect(e.message).to_match('test%.action')
    expect(e.message).to_match('traceback')
  end
end)

test('resolve logs a candidate when client.inspect refuses it', function()
  -- The lab answer to client.inspect {client_id = ""}: bad_request. rex.call may return the error or
  -- raise it; both get a diagnostic line.
  local refusal = {code = 'bad_request', message = 'client ID must start with "client:": ""'}
  local fakes = {
    function() return nil, refusal end,
    function() error(refusal, 0) end,
  }
  for _, f in ipairs(fakes) do
    local got = with_fake({['target.resolve'] = resolved(''), ['client.inspect'] = f},
      function() return H.resolve('lazyrex-testview:v1') end)
    expect(got.value).to_be(nil)
    expect(got.lines).to_have_length(1)
    expect(got.lines[1]).to_match('refused candidate ""')
    expect(got.lines[1]).to_match('bad_request')
  end
end)

test('resolve logs a malformed candidate that Rex calls disconnected', function()
  local got = with_fake({['target.resolve'] = resolved('abc'),
    ['client.inspect'] = {client = {client_id = 'abc', connection_state = 'disconnected'}}},
    function() return H.resolve('lazyrex-testview:v1') end)
  expect(got.value).to_be(nil)
  expect(got.lines).to_have_length(1)
  expect(got.lines[1]).to_match('refused candidate "abc"')
end)

test('resolve skips a disconnected client without a diagnostic', function()
  local got = with_fake({['target.resolve'] = resolved('client:old'),
    ['client.inspect'] = {client = {client_id = 'client:old', connection_state = 'disconnected'}}},
    function() return H.resolve('lazyrex-testview:v1') end)
  expect(got.value).to_be(nil)
  expect(got.lines).to_have_length(0)
end)

test('resolve leaves out excluded clients before it counts the connected ones', function()
  -- Two connected clients share the label: a retired one and a new one (harness.lua, retired).
  local both = {results = {{status = 'ambiguous', candidates = {{client_id = 'client:old'}, {client_id = 'client:new'}}}}}
  local inspect_any = function(p) return {client = {client_id = p.client_id, connection_state = 'connected'}} end
  local got = with_fake({['target.resolve'] = both, ['client.inspect'] = inspect_any},
    function() return H.resolve('lazyrex-testagent:x') end)
  expect(got.value, 'two connected clients: no answer').to_be(nil)
  got = with_fake({['target.resolve'] = both, ['client.inspect'] = inspect_any},
    function() return H.resolve('lazyrex-testagent:x', {['client:old'] = true}) end)
  expect(got.value).to_be('client:new')
  got = with_fake({['target.resolve'] = both, ['client.inspect'] = inspect_any},
    function() return H.resolve('lazyrex-testagent:x', {['client:old'] = true, ['client:new'] = true}) end)
  expect(got.value).to_be(nil)
end)

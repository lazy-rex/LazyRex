-- H.wait_disconnected and Agent:exit (lab/test/harness.lua): only not_found or a connection_state
-- other than connected proves that a client is gone. client.inspect runs through H.rex_call, so a
-- test replaces it with a fake Rex; captures go into tables (docs/testing.md, "Waiting").

-- with_inspect runs fn while client.inspect answers from answers (one function per call, the
-- last one repeats), and gives every other method to the real Rex.
local function with_inspect(answers, fn)
  local real = H.rex_call
  local seen = {n = 0}
  H.rex_call = function(method, payload)
    if method ~= 'client.inspect' then return real(method, payload) end
    seen.n = seen.n + 1
    return (answers[seen.n] or answers[#answers])(payload, real)
  end
  local ok, v = pcall(fn)
  H.rex_call = real
  if not ok then error(v, 0) end
  return seen.n, v
end

local function timeout() return nil, 'stream response "control-1" failed: deadline (status=504 code=timeout)' end
local function connected(p) return {client = {client_id = p.client_id, connection_state = 'connected'}} end
local function gone() return nil, {code = 'not_found', message = 'client not found'} end
local function real(p, call) return call('client.inspect', p) end

test('wait_disconnected retries an error and connected, and ends at not_found', function()
  local n = with_inspect({timeout, connected, gone}, function() return H.wait_disconnected('client:x', 'x', 5) end)
  expect(n).to_be(3)
end)

test('wait_disconnected ends at a connection_state other than connected', function()
  local lost = function(p) return {client = {client_id = p.client_id, connection_state = 'lost'}} end
  local n = with_inspect({connected, lost}, function() return H.wait_disconnected('client:x', 'x', 5) end)
  expect(n).to_be(2)
end)

test('wait_disconnected never takes an error or a malformed answer as gone', function()
  local raised = function() error({code = 'timeout', message = 'deadline'}, 0) end
  local nothing = function() return nil, nil end
  local other = function() return {client = {client_id = 'client:other', connection_state = 'disconnected'}} end
  for _, f in ipairs({timeout, raised, nothing, other, connected}) do
    local e = expect_error(function() with_inspect({f}, function() return H.wait_disconnected('client:x', 'x', 1) end) end, 'timeout')
    expect(e.message).to_match('x to disconnect')
  end
end)

test('exit waits through an inspect error until the client is gone', function()
  -- Fails first on 77020e9: its exit took the first error as a disconnect (one inspection).
  local a = agent('exit-flaky', {join = false})
  local client = a.client
  local n = with_inspect({timeout, connected, real}, function() a:exit() end)
  expect(n >= 3, 'inspections: ' .. n).to_be(true)
  local ins, err = rex.call('client.inspect', {client_id = client})
  expect(err ~= nil or ins.client.connection_state ~= 'connected', 'the client is gone').to_be(true)
end)

test('exit keeps the agent when no disconnect is seen', function()
  local a = agent('exit-stuck', {join = false})
  expect_error(function() with_inspect({timeout}, function() a:exit() end) end, 'disconnect')
  expect(agent('exit-stuck', {join = false}), 'the failed exit keeps the cache').to_be(a)
end)

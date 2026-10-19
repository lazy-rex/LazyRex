-- user:view selection (harness.lua User:view), without timing: a scoped H.views stub controls
-- the snapshots, and the selected view is a real lab renderer.

local real

-- with_views runs fn while H.views returns snapshots(n) for its n-th call (nil: the real list).
-- H.views is restored even when fn fails.
local function with_views(snapshots, fn)
  local saved, n = H.views, 0
  H.views = function(plugin)
    n = n + 1
    return snapshots(n) or saved(plugin)
  end
  local ok, err = pcall(fn)
  H.views = saved
  if not ok then error(err, 0) end
  return n
end

local function copy(t, extra)
  local c = {}
  for k, v in pairs(t) do c[k] = v end
  for k, v in pairs(extra) do c[k] = v end
  return c
end

test('setup: a real view', function()
  user:open('home')
  for _, w in ipairs(H.views()) do if w.tab == 'home' then real = w end end
  expect(real and real.attached).to_be(true)
end)

test('an older attached match and a newer unattached one: wait for the newer one', function()
  local old = copy(real, {view_id = 'view-old-replaced', attached = true})
  local v
  local calls = with_views(function(n)
    if n <= 3 then return {old, copy(real, {attached = false})} end
  end, function() v = user:view('home') end)
  expect(v.id, 'the newest match, not the older attached one').to_be(real.view_id)
  expect(calls > 3, 'it waited while the newer view was not attached').to_be(true)
end)

test('the session filter applies before the newest match', function()
  local other = copy(real, {view_id = 'view-other-session', session_id = 'session:other', attached = true})
  local v
  with_views(function() return {real, other} end, function() v = user:view('home', nil, real.session_id) end)
  expect(v.id, 'a newer view in another session is not selected').to_be(real.view_id)
end)

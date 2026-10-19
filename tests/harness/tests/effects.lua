-- Navigate effects, also to another plugin's tab: the view worker navigates, and the outcome is
-- the operation's effect outcome.

local function committed()
  local out = {}
  for _, e in ipairs(H.hub('hub.test.events', {event = 'navigation_committed'}).events or {}) do out[#out + 1] = e.plugin .. '/' .. e.tab end
  return out
end

test('a navigate effect to another plugin succeeds', function()
  local home = user:open('home')
  home:render()
  local op = home:action('cross_nav', 'go', {})
  expect(op.status).to_be('succeeded')
  expect(op.effects).to_be('done')
  local navs = committed()
  expect(navs[#navs]).to_be('lazyrex/home')
end)

test('a navigate effect to a missing plugin fails the effect', function()
  local op = user:view('home'):action('cross_missing', 'go', {})
  expect(op.status).to_be('succeeded')
  expect(op.effects).to_be('failed')
  expect(op.effects_error).to_match('nope')
end)

test('a failed navigate effect succeeds on retry once the tab can open', function()
  local home = user:view('home')
  local op = home:action('gated_nav', 'go', {})
  expect(op.effects).to_be('failed')
  agent('alice'):call('fix.put', {key = 'gate', value = true})
  local again = home:effect_retry(op.operation_key)
  expect(again.status).to_be('succeeded')
  expect(again.effects).to_be('done')
  expect(tab('gated/a')).to_be_truthy()
end)

test('a cross-plugin navigate effect closes the form that ran it', function()
  local home = user:view('home')
  expect(home:action('open_form', 'open', {}).effects).to_be('done')
  local form = user:view('/forms/fix')
  form:render()
  local op = form:action('go_home', 'go', {})
  expect(op.effects).to_be('done')
  wait('the form view to close', function()
    for _, w in ipairs(views()) do if w.route == '/forms/fix' then return false end end
    return true
  end)
  local navs = committed()
  expect(navs[#navs]).to_be('lazyrex/home')
end)

-- Reviewer probe (Codex, reviewer_navigation.lua): the LazyRex home plugin navigates through the
-- effect path, so a failed target reaches the durable operation. This hub has no messaging plugin.
test('the home plugin reports a failed navigation target on the operation', function()
  agent('alice')
  local home = user:open('home', 'lazyrex')
  home:render()
  local op = home:action('agents', 'message', {item_id = 'alice'}, 'home-missing-target')
  expect(op.status).to_be('succeeded')
  expect(op.effects).to_be('failed')
  expect(op.effects_error).to_match('messaging')
end)

-- Ported from the Node test home.mjs (coverage map: tests/home/README.md): navigation from the
-- home tab. Navigation is user_only ('navigation'): it changes only what the user sees, so it has
-- no agent path. The effects are navigate and panel effects that the hub runs and records.

local codex = agent('codex')

local function home()
  local v = user:open('home')
  v:render()
  return v
end

test('a plugin with a tab: open is a navigate effect to its first tab', function()
  local op = home():action('plugins', 'open', {item_id = 'harnessfix'}, 'nav-tab')
  expect(op.status).to_be('succeeded')
  expect(op.result).to_equal({navigate = {plugin = 'harnessfix', tab = 'home'}})
  user:view('home', 'harnessfix')
end)

test('a plugin with a landing panel: open is a panel effect', function()
  local op = home():action('plugins', 'open', {item_id = 'messaging'}, 'nav-panel')
  expect(op.status).to_be('succeeded')
  expect(op.result).to_equal({panel = {plugin = 'messaging', command = 'threads'}})
  user:view('/panels/threads', 'messaging')
end)

test('message an agent: a navigate effect to its messaging thread', function()
  local op = home():action('agents', 'message', {item_id = 'codex'}, 'nav-message')
  expect(op.status).to_be('succeeded')
  expect(op.result).to_equal({navigate = {plugin = 'messaging', tab = 'thread/codex'}})
  expect(tab('thread/codex', 'messaging'), 'the messaging plugin opened the thread tab').to_be_truthy()
end)

test('open a worker: the hub shows its tab', function()
  local w = codex:call('hub.spawn', {profile = 'shell', key = 'nav-worker', title = 'Navigated'})
  local op = home():action('workers', 'open', {item_id = w.spawn_id}, 'nav-worker-open')
  expect(op.status).to_be('succeeded')
  expect(op.result).to_equal({spawn_id = w.spawn_id, status = 'navigating'})
end)

test('an item that is gone fails with a refresh hint', function()
  local v = home()
  for _, c in ipairs({{'plugins', 'open'}, {'agents', 'message'}, {'workers', 'open'}}) do
    local op = v:action(c[1], c[2], {item_id = 'missing'}, 'nav-missing-' .. c[1])
    expect(op.status).to_be('failed')
    expect(op.error.message).to_match('no longer available')
  end
end)

test('the home plugin cannot be opened from its own list', function()
  local op = home():action('plugins', 'open', {item_id = 'lazyrex'}, 'nav-self')
  expect(op.status).to_be('failed')
  expect(op.error.message).to_match('no landing view')
end)

test('a launch whose navigation fails still reports the launch and shows a notice', function()
  local id = fault({point = 'action', action = 'hub.navigate.worker', when = 'before', mode = 'fail'})
  local op = home():action('launch/shell', 'launch', {values = {title = 'No navigation'}}, 'nav-launch-fail')
  expect(fault_fired(id)).to_be(true)
  expect(op.status).to_be('succeeded')
  expect(op.result.status, 'a navigation failure does not report a failed launch').to_be('open')
  expect(H.node(home():render(), 'navigation_notice').text).to_match('worker started')
  local again = home():action('launch/shell', 'launch', {values = {title = 'Navigated'}}, 'nav-launch-ok')
  expect(again.status).to_be('succeeded')
  expect(H.node(home():render(), 'navigation_notice'), 'the next launch clears the notice').to_be(nil)
end)

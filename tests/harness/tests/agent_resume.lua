-- Agent resume in a new pane (hub.admin.resume; docs/hub.md, "Agent resume"). A fake agent CLI
-- (../fake-agent.sh) reports its identity at its start (as Claude) or at its first prompt (as Codex).
-- The kind of the identity decides the hub's first prompt (harness.go resumePrompt): Codex gets the
-- neutral text, Claude gets none.

local alice = agent('alice')
local fake = H.args.file:match('^(.*)/tests/[^/]*$') .. '/fake-agent.sh'
local lab = H.args.lab
local neutral = 'LazyRex: this conversation was resumed in a new pane.'

local function profile(id, resume_when, delay, kind)
  kind = kind or 'codex'
  return {id = id, name = id, kind = 'agent', cwd = lab,
    command = {'/bin/sh', fake, kind, '${conv}', 'startup', 'start'}, args = {{name = 'conv', required = true}},
    resume = {'/bin/sh', fake, kind, '${conversation_id}', 'resume', resume_when, delay or '0'}}
end

write_profiles({profiles = {profile('codexlike', 'prompt'), profile('claudelike', 'start'),
    profile('claude', 'start', '0', 'claude'), profile('codex', 'prompt'),
    profile('silent', 'never'), profile('late', 'prompt', '4'), profile('foreign', 'foreign'),
    {id = 'noresume', name = 'noresume', kind = 'agent', cwd = lab,
      command = {'/bin/sh', fake, 'codex', '${conv}', 'startup', 'start'}, args = {{name = 'conv', required = true}}}}})

-- lines is the input log of the fake agents of a conversation.
local function lines(conv)
  local out = {}
  local f = io.open(lab .. '/fake-' .. conv .. '.log')
  if not f then return out end
  for l in f:lines() do out[#out + 1] = l end
  f:close()
  return out
end

-- count counts the log lines of conv with text.
local function count(conv, text)
  local n = 0
  for _, l in ipairs(lines(conv)) do if l:find(text, 1, true) then n = n + 1 end end
  return n
end

-- prompts counts the first prompts that reached the fake agents of a conversation.
local function prompts(conv)
  return count(conv, neutral)
end

local function identity(id)
  for _, i in ipairs(H.hub('hub.identity.list', {}).identities or {}) do
    if i.identity == id then return i end
  end
end

local function write_block(sess, block, data)
  local r, err = rex.call('block.method', {session_id = sess, block_id = block, method = 'write', args = {data = data}})
  if err ~= nil then error('write: ' .. tostring(err), 0) end
  return r
end

-- started spawns a fake agent through hub.spawn and waits until its identity (kind, default codex)
-- is current.
local function started(prof, conv, kind)
  local s = alice:hub('hub.spawn', {profile = prof, key = 'spawn-' .. conv, args = {conv = conv}})
  local id = (kind or 'codex') .. ':' .. conv
  H.wait('the identity of ' .. conv, function()
    local i = identity(id)
    return i and i.state == 'current' and i.block_id == s.block_id
  end)
  return id, s.block_id, s.session_id
end

-- detached ends the fake agent; its pane stays (exit on_completion false), the identity detaches.
local function detached(id, block, sess)
  write_block(sess, block, 'exit\r')
  H.wait('the identity detached', function() local i = identity(id); return i and i.state == 'detached' end)
end

local function resume(args)
  return H.hub('hub.admin.resume', args)
end

local function finished(key, args)
  args = args or {}
  args.key = key
  return H.wait('operation ' .. key, function()
    local r = resume(args)
    return r.done and r
  end, 30)
end

local function target(r, id)
  for _, t in ipairs(r.targets) do if t.agent == id then return t end end
end

test('a Codex-like agent resumes in a new pane: one first prompt, then its hook rebinds it', function()
  local id, old, sess = started('codexlike', 'c1')
  detached(id, old, sess)
  local r = finished('r-c1', {agent = id})
  local t = target(r, id)
  expect(t.phase).to_be('rebound')
  expect(t.block_id).not_to_be(old)
  local i = identity(id)
  expect(i.state).to_be('current')
  expect(i.block_id).to_be(t.block_id)
  -- The same key replays the operation: no second pane, no second prompt.
  local again = resume({key = 'r-c1', agent = id})
  expect(again.op_id).to_be(r.op_id)
  expect(target(again, id).block_id).to_be(t.block_id)
  expect(prompts('c1')).to_be(1)
  -- The key with another request is refused.
  H.expect_error(function() resume({key = 'r-c1', kind = 'codex'}) end, 'different request')
end)

test('a Claude-like agent rebinds at its start; the first prompt is not needed', function()
  local id, old, sess = started('claudelike', 'c2')
  detached(id, old, sess)
  local t = target(finished('r-c2', {agent = id}), id)
  expect(t.phase).to_be('rebound')
  expect(identity(id).block_id).to_be(t.block_id)
end)

test('a Claude agent rebinds at its start and gets no first prompt from the hub', function()
  local id, old, sess = started('claude', 'c9', 'claude')
  detached(id, old, sess)
  local t = target(finished('r-c9', {agent = id}), id)
  expect(t.phase).to_be('rebound')
  expect(identity(id).block_id).to_be(t.block_id)
  rex.sleep(1)
  expect(prompts('c9')).to_be(0)
end)

-- notified commits one unread plugin notice for id, as Messaging does for a new message.
local function notified(id, notice)
  local r = alice:call('fix.notify', {notices = {notice or {agent = id, pending = true, once = true}}})
  expect(r.error, 'the notice commit').to_be(nil)
end

-- at is the index of the first log line of conv with text, or nil.
local function at(conv, text)
  for i, l in ipairs(lines(conv)) do if l:find(text, 1, true) then return i end end
end

-- lead-harness2 item 4: an unread notice of a detached agent wakes it in its new pane, after the
-- rebind. The hub's own prompt names no plugin.
for _, c in ipairs({{kind = 'claude', prof = 'claude', conv = 'n1', first = 0}, {kind = 'codex', prof = 'codexlike', conv = 'n2', first = 1}}) do
  test('a resumed ' .. c.kind .. ' agent with one unread notice gets the plugin notice after the rebind', function()
    local id, old, sess = started(c.prof, c.conv, c.kind)
    detached(id, old, sess)
    notified(id)
    local t = target(finished('r-' .. c.conv, {agent = id}), id)
    expect(t.phase).to_be('rebound')
    H.wait('the plugin notice in the new pane', function() return count(c.conv, 'Fixture notice.') == 1 end, 20)
    expect(prompts(c.conv)).to_be(c.first)
    if c.first == 1 then
      expect(at(c.conv, neutral) < at(c.conv, 'Fixture notice.'), 'the neutral prompt comes first').to_be(true)
    end
    rex.sleep(1.5)
    expect(count(c.conv, 'Fixture notice.'), 'one notice').to_be(1)
  end)
end

test('a read notice gives no nudge after the resume (the once rule)', function()
  local id, old, sess = started('claude', 'n3', 'claude')
  detached(id, old, sess)
  notified(id)
  notified(id, {agent = id, read = true})
  local t = target(finished('r-n3', {agent = id}), id)
  expect(t.phase).to_be('rebound')
  rex.sleep(3)
  expect(count('n3', 'Fixture notice.')).to_be(0)
end)

test('refusals: a current agent, no resume template, no agent', function()
  local id, block, sess = started('noresume', 'c3')
  local r = resume({key = 'r-c3', agent = id})
  expect(target(r, id).phase).to_be('refused')
  expect(target(r, id).detail).to_match('not detached')
  write_block(sess, block, 'exit\r')
  H.wait('c3 detached', function() return identity(id).state == 'detached' end)
  r = resume({key = 'r-c3b', agent = id})
  expect(target(r, id).detail).to_match('no resume template')
  H.expect_error(function() resume({key = 'r-none', agent = 'codex:nobody'}) end, 'no agent identity')
  H.expect_error(function() resume({key = 'r-bad', agent = id, kind = 'codex'}) end, 'give agent')
end)

test('one active operation for each agent; a release ends it; an older pane cannot complete a newer attempt', function()
  local id, old, sess = started('late', 'c4')
  detached(id, old, sess)
  -- The late fake reports 4 s after its prompt: the first operation waits in prompt_sent.
  resume({key = 'r-c4a', agent = id})
  H.wait('prompt sent', function() return target(resume({key = 'r-c4a', agent = id}), id).phase == 'prompt_sent' end, 20)
  local busy = resume({key = 'r-c4b', agent = id})
  expect(target(busy, id).phase).to_be('refused')
  expect(target(busy, id).detail).to_match('busy')
  local rel = resume({key = 'r-c4rel', release = id})
  expect(target(rel, id).detail).to_match('released operation')
  local firstPane = target(resume({key = 'r-c4a', agent = id}), id).block_id
  expect(target(resume({key = 'r-c4a', agent = id}), id).phase).to_be('released')
  -- A new attempt. The first pane's late report comes first and rebinds the identity there; it
  -- does not complete the new attempt, whose own pane rebinds it after its own report.
  resume({key = 'r-c4c', agent = id})
  H.wait('the late first pane reported', function() return identity(id).block_id == firstPane end, 20)
  local t = target(resume({key = 'r-c4c', agent = id}), id)
  expect(t.phase).not_to_be('rebound')
  t = target(finished('r-c4c', {agent = id}), id)
  expect(t.phase).to_be('rebound')
  expect(t.block_id).not_to_be(firstPane)
  expect(identity(id).block_id).to_be(t.block_id)
end)

test('a lost create answer is unknown: the reservation stays, a retry launches nothing, a release frees it', function()
  local id, old, sess = started('silent', 'c5')
  detached(id, old, sess)
  local f = H.fault({point = 'spawn-create', mode = 'lose-answer'})
  local r = H.wait('unknown', function()
    local x = resume({key = 'r-c5', agent = id})
    return target(x, id).phase == 'unknown' and x
  end, 20)
  expect(H.fault_fired(f)).to_be(true)
  expect(r.done).to_be(false)
  local spawn = target(r, id).spawn_id
  expect(target(resume({key = 'r-c5', agent = id}), id).spawn_id).to_be(spawn)
  expect(target(resume({key = 'r-c5x', agent = id}), id).detail).to_match('busy')
  resume({key = 'r-c5rel', release = id})
  expect(target(resume({key = 'r-c5', agent = id}), id).phase).to_be('released')
end)

test('the prompt goes only to the launched program: a status from another process gives unknown and no prompt', function()
  local id, old, sess = started('foreign', 'c6')
  detached(id, old, sess)
  local t = H.wait('unknown', function()
    local x = target(resume({key = 'r-c6', agent = id}), id)
    return x.phase == 'unknown' and x
  end, 20)
  expect(t.detail).to_match('owner')
  expect(prompts('c6')).to_be(0)
  -- The reservation stays.
  expect(target(resume({key = 'r-c6b', agent = id}), id).detail).to_match('busy')
end)

test('a lost answer of the prompt write: unknown, never written again; the hook still rebinds', function()
  local id, old, sess = started('codexlike', 'c7')
  detached(id, old, sess)
  local f = H.fault({point = 'resume-prompt', mode = 'lose-answer', key = id})
  local t = target(finished('r-c7', {agent = id}), id)
  expect(H.fault_fired(f)).to_be(true)
  -- The prompt reached the agent: its hook rebound it, which ends the unknown target.
  expect(t.phase).to_be('rebound')
  expect(prompts('c7')).to_be(1)
  resume({key = 'r-c7', agent = id})
  expect(prompts('c7')).to_be(1)
end)

-- hand_started starts a fake Codex in a new tab of sess without hub.spawn, as a user starts an agent
-- by hand: its identity has no spawn record. It returns the identity and its block.
local function hand_started(sess, conv)
  local r, err = rex.call('session.new_window', {session_id = sess, focus = false, window_label = 'hand ' .. conv,
    layout = {block = {flavor = 'com.superlogical.terminal.shell', label = 'hand-' .. conv,
      options = {command = {'/bin/sh', fake, 'codex', conv, 'startup', 'start'}, cwd = lab, exit = {on_completion = false}}}}})
  if err ~= nil then error('new_window: ' .. tostring(err), 0) end
  local block = r.block_ids[1]
  local id = 'codex:' .. conv
  H.wait('the identity of ' .. conv, function()
    local i = identity(id)
    return i and i.state == 'current' and i.block_id == block
  end)
  return id, block
end

-- window_of is the window of sess whose layout holds block, or nil.
local function window_of(sess, block)
  local function holds(x)
    if type(x) ~= 'table' then return false end
    if x.block_id == block then return true end
    for _, v in pairs(x) do if holds(v) then return true end end
    return false
  end
  local v, err = rex.call('session.view', {session_id = sess})
  if err ~= nil then error('session.view: ' .. tostring(err), 0) end
  for _, w in ipairs(v.windows or {}) do
    if holds(w.layers) or holds(w.layout) then return w.window_id end
  end
end

test('an agent started by hand resumes beside a pane in the given cwd; its next resume uses its spawn cwd', function()
  local _, anchor, sess = started('codexlike', 'h0') -- a placed pane to resume beside
  local id, block = hand_started(sess, 'h1')
  detached(id, block, sess)
  local t = target(finished('r-h1-nocwd', {agent = id}), id)
  expect(t.phase).to_be('refused')
  expect(t.detail).to_be('no spawn record and no cwd')
  local r = finished('r-h1', {agent = id, cwd = lab, placement = {session = sess, beside = anchor, direction = 'right', ratio = 0.3}})
  t = target(r, id)
  expect(t.phase).to_be('rebound')
  expect(t.cwd).to_be_truthy()
  expect(identity(id).block_id).to_be(t.block_id)
  expect(window_of(sess, t.block_id)).to_be(window_of(sess, anchor))
  expect(prompts('h1')).to_be(1) -- the kind's first prompt (Codex)
  -- Now it has a spawn record: a second resume uses that spawn's cwd, and another cwd is not used.
  local first = t
  detached(id, first.block_id, sess)
  t = target(finished('r-h1-2', {agent = id, cwd = '/'}), id)
  expect(t.phase).to_be('rebound')
  expect(t.cwd).to_be(first.cwd)
end)

test('a registered plugin resumes one agent for the user; without for, the batch form and an agent are refused', function()
  local _, _, sess = started('codexlike', 'h2a')
  local id, block = hand_started(sess, 'h2')
  detached(id, block, sess)
  local function as_plugin(args) return alice:call('fix.hub', {action = 'hub.admin.resume', args = args}) end
  local r = as_plugin({key = 'p-h2-nofor', agent = id, cwd = lab})
  expect(r.error and r.error.message or '').to_contain('refused from agent and plugin panes')
  r = as_plugin({key = 'p-h2-batch', kind = 'codex', ['for'] = 'user'})
  expect(r.error and r.error.code or '').to_be('invalid_input')
  H.expect_error(function() alice:hub('hub.admin.resume', {key = 'a-h2', agent = id, cwd = lab, ['for'] = 'user'}) end,
    'refused from agent and plugin panes')
  local args = {key = 'p-h2', agent = id, cwd = lab, ['for'] = 'user'}
  r = H.wait('the plugin resume', function()
    local x = as_plugin(args)
    expect(x.error).to_be(nil)
    return x.result.done and x.result
  end, 30)
  expect(target(r, id).phase).to_be('rebound')
  expect(identity(id).state).to_be('current')
end)

test('a hub stop while the prompt is being written: unknown after the restart, no write, the reservation stays', function()
  local id, old, sess = started('codexlike', 'c8')
  detached(id, old, sess)
  H.fault({point = 'resume-prompt', mode = 'hold', key = id, hold_ms = 20000})
  H.wait('prompt_sending', function() return target(resume({key = 'r-c8', agent = id}), id).phase == 'prompt_sending' end, 20)
  H.restart_hub({crash = true})
  local t = H.wait('unknown', function()
    local x = target(resume({key = 'r-c8', agent = id}), id)
    return x.phase == 'unknown' and x
  end, 20)
  expect(t.detail).to_match('hub stopped')
  rex.sleep(3)
  expect(prompts('c8')).to_be(0)
  expect(target(resume({key = 'r-c8', agent = id}), id).phase).to_be('unknown')
  expect(target(resume({key = 'r-c8b', agent = id}), id).detail).to_match('busy')
  resume({key = 'r-c8rel', release = id})
  expect(target(resume({key = 'r-c8', agent = id}), id).phase).to_be('released')
end)

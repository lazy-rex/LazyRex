-- Agent prompts on the real hub (docs/hub.md, "Agent prompts and acting as the user"): prompts
-- from OSC 7501 reports, pushes, hub.prompts, and hub.prompt.answer writing to a real pane.

local function pf(action, args)
  local st = H.hub('hub.test.status', {})
  return H.perform(st.plugins.promptfix.client_id, action, args or {})
end

local function call(action, args)
  local r = pf('pf.hub', {action = action, args = args})
  if r.error then error(r.error.message, 0) end
  return r.result
end

local function try(action, args) return pf('pf.hub', {action = action, args = args}) end

-- prompt_of waits until the plugin's applied state has an open prompt for agent with this msg.
local function prompt_of(agent, msg)
  return wait('a prompt of ' .. agent .. ' with msg ' .. msg, function()
    local p = pf('pf.state', {}).prompts[agent]
    if p and p.msg == msg then return p end
  end)
end

local function no_prompt(agent)
  wait('no prompt of ' .. agent, function() return pf('pf.state', {}).prompts[agent] == nil end)
end

local function answer(id, key, input) return call('hub.prompt.answer', {prompt_id = id, key = key, input = input}) end

test('a blocked report opens a prompt; pushes and hub.prompts give it; a state change closes it', function()
  local alice = agent('alice')
  alice:program_status('working', {app = 'claude-code'})
  alice:program_status('blocked', {kind = 'permission', app = 'claude-code', msg = 'Allow Bash: ls?'})
  local p = prompt_of('alice', 'Allow Bash: ls?')
  expect(p.kind).to_be('permission')
  expect(p.app).to_be('claude-code')
  expect(p.block_id).to_match('^block:')
  local snap = call('hub.prompts', {})
  expect(snap.hub_generation).to_be(pf('pf.state', {}).generation)
  expect(#snap.prompts).to_be(1)
  expect(snap.prompts[1].prompt_id).to_be(p.prompt_id)
  local listed
  for _, a in ipairs(H.hub('hub.agents', {}).agents) do
    if a.id == 'alice' then listed = a end
  end
  expect(listed.block_id).to_be(p.block_id)
  -- The same report keeps the prompt; another msg replaces it.
  alice:program_status('blocked', {kind = 'permission', app = 'claude-code', msg = 'Allow Bash: ls?'})
  alice:program_status('blocked', {kind = 'permission', app = 'claude-code', msg = 'Allow Edit?'})
  local q = prompt_of('alice', 'Allow Edit?')
  expect(q.prompt_id).not_to_be(p.prompt_id)
  expect(q.generation).to_be(p.generation + 1)
  alice:program_status('working', {app = 'claude-code'})
  no_prompt('alice')
  expect(#call('hub.prompts', {}).prompts).to_be(0)
end)

test('a blocked report without kind and msg (Codex) opens a prompt', function()
  local bob = agent('bob')
  bob:program_status('blocked', {app = 'codex'})
  local p = wait('the Codex prompt', function() return pf('pf.state', {}).prompts.bob end)
  expect(p.kind).to_be('')
  expect(p.msg).to_be('')
  bob:program_status('idle', {app = 'codex'})
  no_prompt('bob')
end)

test('answers write to the pane: keys with no Enter, text as a paste and Enter', function()
  local alice = agent('alice')
  alice:input(true) -- start the capture
  alice:program_status('blocked', {kind = 'question', msg = 'Pick one'})
  local p = prompt_of('alice', 'Pick one')
  local r = answer(p.prompt_id, 'k-keys', {keys = '2'})
  expect(r.phase).to_be('done')
  wait('the key', function() return alice:input() == '2' end)
  alice:input(true)
  r = answer(p.prompt_id, 'k-text', {text = 'blue\nplease', submit = true})
  expect(r.phase).to_be('done')
  wait('the paste and Enter', function() return alice:input() == '\27[200~blue please\27[201~\r' end)
  alice:input(true)
  -- A repeat returns the first result and writes nothing; other input with the key is refused.
  expect(answer(p.prompt_id, 'k-keys', {keys = '2'}).phase).to_be('done')
  local e = try('hub.prompt.answer', {prompt_id = p.prompt_id, key = 'k-keys', input = {keys = '3'}})
  expect(e.error.code).to_be('operation_conflict')
  -- A closed prompt: refused, zero bytes.
  alice:program_status('working', {})
  no_prompt('alice')
  r = answer(p.prompt_id, 'k-late', {keys = '1'})
  expect(r.phase).to_be('refused')
  expect(r.result.reason).to_be('prompt_closed')
  rex.sleep(0.3)
  expect(alice:input()).to_be('')
end)

test('a close at a binding check (fault): zero bytes before the write, no Enter after the paste', function()
  local alice = agent('alice')
  alice:input(true)
  alice:program_status('blocked', {kind = 'question', msg = 'Fault one'})
  local p = prompt_of('alice', 'Fault one')
  local f = fault({point = 'prompt-answer', mode = 'close', when = 'before-write'})
  local r = answer(p.prompt_id, 'f-1', {keys = '1'})
  expect(fault_fired(f)).to_be(true)
  expect(r.phase).to_be('refused')
  -- The record is still blocked: the next report opens a new prompt.
  alice:program_status('blocked', {kind = 'question', msg = 'Fault two'})
  local q = prompt_of('alice', 'Fault two')
  f = fault({point = 'prompt-answer', mode = 'close', when = 'before-enter'})
  r = answer(q.prompt_id, 'f-2', {text = 'red', submit = true})
  expect(r.phase).to_be('partial')
  expect(r.result.enter).to_be('not_sent')
  wait('the paste only', function() return alice:input() == '\27[200~red\27[201~' end)
  alice:program_status('working', {})
end)

test('a write that Rex refuses with not_found is failed; a lost Enter reply is unknown_after_text', function()
  local alice = agent('alice')
  alice:input(true)
  alice:program_status('blocked', {kind = 'question', msg = 'Errors'})
  local p = prompt_of('alice', 'Errors')
  fault({point = 'prompt-answer', mode = 'refuse', when = 'write'})
  local r = answer(p.prompt_id, 'e-1', {keys = '1'})
  expect(r.phase).to_be('failed')
  expect(r.result.code).to_be('not_found')
  fault({point = 'prompt-answer', mode = 'lose-reply', when = 'enter'})
  r = answer(p.prompt_id, 'e-2', {text = 'x', submit = true})
  expect(r.phase).to_be('unknown_after_text')
  wait('the paste and Enter were written', function() return alice:input() == '\27[200~x\27[201~\r' end)
  alice:program_status('working', {})
end)

test('a lost push reply, then a close: the plugin ends with no prompt', function()
  local alice = agent('alice')
  pf('pf.fail', {n = 1})
  alice:program_status('blocked', {kind = 'question', msg = 'Lost'})
  prompt_of('alice', 'Lost') -- applied, but the hub saw a failure
  alice:program_status('working', {})
  no_prompt('alice')
end)

test('a hub restart: a new generation, every prompt closed; a repeated answer returns its record', function()
  local alice = agent('alice')
  alice:input(true)
  alice:program_status('blocked', {kind = 'question', msg = 'Before restart'})
  local p = prompt_of('alice', 'Before restart')
  expect(answer(p.prompt_id, 'r-1', {keys = '1'}).phase).to_be('done')
  wait('the key', function() return alice:input(true) == '1' end)
  local old = pf('pf.state', {}).generation
  hub:restart()
  wait('the plugin registered with the new generation', function()
    local g = pf('pf.state', {}).generation
    return g and g ~= old
  end)
  -- H1d: the key check comes first, so the old prompt_id gives the recorded result.
  local r = answer(p.prompt_id, 'r-1', {keys = '1'})
  expect(r.phase).to_be('done')
  rex.sleep(0.3)
  expect(alice:input()).to_be('')
  -- The record is still blocked: the new hub opens a new prompt with a new id.
  local snap = wait('a prompt in the new hub', function()
    local s = call('hub.prompts', {})
    if #s.prompts == 1 then return s end
  end)
  expect(snap.prompts[1].prompt_id).not_to_be(p.prompt_id)
  alice:program_status('working', {})
end)

-- Last: its revisions are far above the real ones, so its snapshot floor makes the fixture ignore
-- every later real push of this hub run.
test('the apply rule: every arrival order of pushes and snapshots, and a stale generation', function()
  local gen = pf('pf.state', {}).generation
  local function push(agent, rev, msg)
    return pf('pf.inject', {kind = 'push', msg = {hub_generation = gen, rev = tostring(rev), agent = agent,
      prompt = msg and {prompt_id = msg, agent = agent, msg = msg} or nil}}).outcome
  end
  local function snapshot(rev, list)
    local prompts = {}
    for agent, msg in pairs(list) do prompts[#prompts + 1] = {prompt_id = msg, agent = agent, msg = msg} end
    return pf('pf.inject', {kind = 'snapshot', msg = {hub_generation = gen, rev = tostring(rev), prompts = prompts}}).outcome
  end
  local function has(agent) return pf('pf.state', {}).prompts[agent] ~= nil end
  -- Revisions far above the real ones, so real pushes of this run do not interfere.
  -- A newer open push first, then an older snapshot without it: B stays open.
  push('rule-b', 100011, 'B')
  snapshot(100010, {})
  expect(has('rule-b')).to_be(true)
  -- A newer close push first, then an older snapshot with the prompt open: A stays closed.
  push('rule-a', 100021, nil)
  snapshot(100020, {['rule-a'] = 'A'})
  expect(has('rule-a')).to_be(false)
  -- A snapshot, then a delayed older push for an agent in neither: ignored (the floor).
  snapshot(100030, {})
  expect(push('rule-c', 100029, 'C')).to_be('ignored')
  expect(has('rule-c')).to_be(false)
  -- A newer push after the snapshot applies.
  expect(push('rule-c', 100031, 'C')).to_be('applied')
  expect(has('rule-c')).to_be(true)
  -- A push or a snapshot of another hub generation is ignored and changes nothing.
  local stale = pf('pf.inject', {kind = 'push', msg = {hub_generation = 'stale', rev = '999999', agent = 'rule-c'}}).outcome
  expect(stale).to_be('ignored')
  expect(has('rule-c')).to_be(true)
  expect(pf('pf.state', {}).generation).to_be(gen)
end)

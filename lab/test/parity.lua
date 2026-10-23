-- The parity sweep (docs/testing.md, "Agent parity"). `lazyrex-hub test` runs it first, against its
-- own lab and hub. It renders every declared tab and every palette command of the plugin under
-- test, then asks the hub for the parity report. A required route that no render reached is
-- unchecked, and an unchecked route fails the sweep: it does not pass.
--
-- Contextual routes (a form that only opens from another view) come from the plugin's fixture
-- (manifest test.parity_fixture), which renders them with the test API. Manifest test.parity_routes
-- lists them; each must be rendered.

local plugin = H.args.plugin

if FIXTURE then
  test('fixture renders the contextual routes', function()
    load_fixture(FIXTURE)
  end)
end

test('every declared tab renders', function()
  for _, t in ipairs(tabs()) do
    user:open(t.key):render()
  end
end)

test('every palette command opens and renders', function()
  for _, c in ipairs(H.hub('hub.test.commands', {plugin = plugin}).commands or {}) do
    user:command(c.id)
    user:view(c.route):render()
  end
end)

test('parity report', function()
  local report = H.hub('hub.test.parity', {})
  local problems = {}
  for _, v in ipairs(report.violations or {}) do
    if v.plugin == plugin then
      problems[#problems + 1] = string.format('%s %s %s: %s', v.route or ('command ' .. tostring(v.command)), v.node or '', v.action or '', v.problem)
    end
  end
  local checked = {}
  -- A checked route can carry a query (/forms/worktree?path=...); a declared contextual route is
  -- its path, so a route counts by its path.
  for _, c in ipairs(report.checked or {}) do
    if c.plugin == plugin then checked[c.route:match('^[^?]*')] = true end
  end
  for _, v in ipairs(report.unrendered or {}) do
    if v.plugin == plugin then problems[#problems + 1] = 'unchecked (no render reached it): ' .. v.route end
  end
  for _, r in ipairs(PARITY_ROUTES) do
    if not checked[r] then problems[#problems + 1] = 'unchecked contextual route (the fixture did not render it): ' .. r end
  end
  local lines = {}
  for r in pairs(checked) do lines[#lines + 1] = r end
  table.sort(lines)
  io.stderr:write('parity: checked routes: ' .. table.concat(lines, ', ') .. '\n')
  if #problems > 0 then error('parity failed:\n' .. table.concat(problems, '\n'), 0) end
end)

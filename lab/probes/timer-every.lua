-- Timer probe: rex.every and rex.after in a long-running plugin-like script.
local S = {ticks = 0, after = 0, first = os.time(), last = 0, calls = 0, tick_in_hold = 0, holding = false, caught = 0}
local actions = {}
for _, n in ipairs({'probe.state', 'probe.hold', 'probe.fail'}) do
  actions[#actions + 1] = {name = n, kind = 'client', title = n, args = {type = 'object', additionalProperties = true}}
end
rex.call('client.keymap', {actions = actions})
rex.call('client.set_label', {label = 'timerprobe'})
S.timer = rex.every(2, function(ctx)
  S.ticks = S.ticks + 1
  S.last = os.time()
  S.origin = ctx and ctx.origin
  if S.holding then S.tick_in_hold = S.tick_in_hold + 1 end
end)
rex.after(1, function() S.after = S.after + 1 end)
rex.on('client_perform_requested', function(ctx, ev)
  S.calls = S.calls + 1
  if ev.action == 'probe.hold' then S.holding = true; rex.sleep(7); S.holding = false end
  if ev.action == 'probe.fail' then
    local ok = pcall(error, 'x')
    if not ok then S.caught = S.caught + 1 end
  end
  rex.call('client.reply', {call_id = ev.call_id, result = {ticks = S.ticks, after = S.after, age = os.time() - S.first,
    since_last = S.last > 0 and os.time() - S.last or -1, calls = S.calls, tick_in_hold = S.tick_in_hold, caught = S.caught, origin = S.origin}})
end)

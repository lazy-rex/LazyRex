-- Probe (rex-protocol.md, "Floating layers in the app"): what a floating layer
-- does when its block closes or exits, and where focus goes. It makes its own session and closes it.
-- Run: rex -S <lab> --autostart=false do <this file>
-- Probe: a floating layer with one block; close the block (block.close) and let a second one exit.
local function view(s) local v = rex.call('session.view', {session_id = s}); return v end
local function layers(s)
  local out = {}
  for _, w in ipairs(view(s).windows or {}) do
    for _, l in ipairs(w.layers or {}) do out[#out + 1] = (l.kind or '?') .. ':' .. tostring(l.layer_id) end
  end
  return table.concat(out, ' '), view(s).windows[1].focused_block_id
end
local s = rex.call('session.create', {label = 'layerprobe', initial_windows = {{layout = {block = {flavor = 'com.superlogical.terminal.shell', label = 'base', options = {command = {'sleep', '60'}, login = false}}}}}})
local sid = s.session_id
rex.call('session.attach', {session_id = sid, event_scope = 'session'})
local v = view(sid); local win = v.windows[1].window_id; local base = v.windows[1].focused_block_id
print('start', layers(sid))
local l1 = rex.call('session.new_layer', {session_id = sid, window_id = win, bounds = {0.15, 0.15, 0.7, 0.6}, focus = true,
  layout = {block = {flavor = 'com.superlogical.terminal.shell', label = 'l1', options = {command = {'sleep', '60'}, login = false}}}})
print('new_layer', l1.layer_id, #l1.block_ids)
print('after open', layers(sid))
rex.call('block.close', {session_id = sid, block_id = l1.block_ids[1]})
rex.sleep(0.5)
print('base=' .. base, 'after block.close', layers(sid))
local l2 = rex.call('session.new_layer', {session_id = sid, window_id = win, bounds = {0.15, 0.15, 0.7, 0.6}, focus = true,
  layout = {block = {flavor = 'com.superlogical.terminal.shell', label = 'l2', options = {command = {'true'}, login = false, exit = {on_completion = true}}}}})
rex.sleep(1.5)
print('after exit on_completion', layers(sid))
-- two layers at once
local a = rex.call('session.new_layer', {session_id = sid, window_id = win, bounds = {0.15, 0.15, 0.7, 0.6}, focus = true,
  layout = {block = {flavor = 'com.superlogical.terminal.shell', label = 'a', options = {command = {'sleep', '60'}, login = false}}}})
local b = rex.call('session.new_layer', {session_id = sid, window_id = win, bounds = {0.15, 0.15, 0.7, 0.6}, focus = true,
  layout = {block = {flavor = 'com.superlogical.terminal.shell', label = 'b', options = {command = {'sleep', '60'}, login = false}}}})
print('two layers', layers(sid))
rex.call('block.close', {session_id = sid, block_id = b.block_ids[1]})
rex.sleep(0.5)
print('a=' .. a.block_ids[1], 'close b', layers(sid))
rex.call('session.close', {session_id = sid})

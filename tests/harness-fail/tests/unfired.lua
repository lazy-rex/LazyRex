-- want: fault 1 (spawn-create drop  ) did not fire
-- A fault that never fires fails the file.

test('arms a fault and never triggers it', function()
  fault({point = 'spawn-create', mode = 'drop', op = 'never'})
end)

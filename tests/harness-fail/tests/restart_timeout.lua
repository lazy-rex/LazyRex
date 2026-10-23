-- want: timeout
test('a hub restart and an agent, then a hang past the file timeout', function()
  agent('alice', {report = false})
  hub:restart()
  rex.sleep(600)
end)

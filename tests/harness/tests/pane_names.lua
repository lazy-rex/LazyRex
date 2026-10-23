-- An agent in a tab the user never named is named after its terminal title, without the leading
-- status glyph, and the name follows the title (identity.go paneName). A tab label that the user
-- sets wins over the title (observed on Rex 0.0.1792158932-g88fd41).

test('an unnamed tab names its agent after the terminal title', function()
  local a = agent('quill', {report = false, unnamed = true})
  a:title('\226\156\179 First title') -- U+2733, the status glyph of an agent CLI
  a:report({kind = 'claude', conversation_id = 'quill-names'})
  local id = 'claude:quill-names'
  local function name()
    local list = H.hub('hub.agents', {all = true}).agents or {}
    for _, x in ipairs(list) do
      if x.id == id then return x.name end
    end
    return nil
  end
  wait('the name is the title', function() return name() == 'First title' end)

  -- A change of the status glyph only does not rename; a new title does.
  a:title('\226\160\130 First title') -- U+2802, a spinner frame
  rex.sleep(1.5)
  expect(name(), 'a glyph change keeps the name').to_be('First title')
  a:title('\226\156\179 Second title')
  wait('the name follows the title', function() return name() == 'Second title' end)

  -- A tab label that the user sets wins over the title.
  a:set_label('Named tab')
  wait('the tab label wins', function() return name() == 'Named tab' end)
  a:title('\226\156\179 Third title')
  rex.sleep(2)
  expect(name(), 'a title change does not override the user label').to_be('Named tab')
end)

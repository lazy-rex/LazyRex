-- sql() and sql_write(): a test reads and writes the plugin's SQL database (hub.test.sql).

test('sql_write writes rows that sql reads, with NULL as nil', function()
  sql_write('CREATE TABLE notes (id INTEGER PRIMARY KEY, text TEXT, big INTEGER)')
  local r = sql_write('INSERT INTO notes (text, big) VALUES (?, ?)', {{null = true}, {int = '9007199254740993'}})
  expect(r.changes).to_be(1)
  sql_write('INSERT INTO notes (text, big) VALUES (:text, 2)', {text = 'b'})
  local rows = sql('SELECT id, text, big FROM notes ORDER BY id')
  expect(#rows).to_be(2)
  expect(rows[1].text).to_be(nil)
  expect(rows[1].big).to_be('9007199254740993')
  expect(rows[2].text).to_be('b')
  expect(#sql('SELECT id FROM notes WHERE id = ?', {r.id})).to_be(1)
end)

test('the hub refuses its own tables and a write in sql()', function()
  expect(expect_error(function() sql('SELECT * FROM lazyrex_ops') end).code).to_be('refused')
  expect(expect_error(function() sql('DELETE FROM notes') end).code).to_be('refused')
end)

# Transcript formatter

The transcript pane's message formatter. No hub or terminal calls. No pane width.

`Format(message, previous, dark)` returns one message with a final newline.
It adds a blank line before the header when `previous` is not nil.
The caller passes the previous printed message, and nil for the first message of a rebuild.
The caller supplies the displayed time in `At`; the formatter does not read the clock.

The header has a colored bar (`▍`), the bold sender name, the time, and `no reply needed` for FYI.
User messages (`Own: true`) have bold paragraphs and lists, without a prompt or a background.
User code blocks keep their code color and normal weight. Agent bodies use normal text with Markdown emphasis.

Goldmark parses Markdown. A tree transformation converts tables into labeled records.
The transformation retains cell inline nodes, including emphasis, code, and links.
Glamour's ANSI renderer then formats the transformed tree without width or padding.
Source soft breaks become spaces. Explicit hard breaks and code lines remain breaks.
The formatter preserves source code indentation. Terminal continuation rows do not repeat it.
Input terminal sequences and control characters are removed, including numeric control entities.
On a renderer error, the formatter uses sanitized literal message text.

The palette uses blue for the user, orange for Claude, green for Codex, and purple for other senders.
Sender matching for colors ignores case. User messages take priority over the author name.
The `dark` argument selects separate light and dark palettes. Body and code colors are explicit.
The printer owns theme detection and passes the selected palette for each message.
Readability in the native app remains unverified.

`TestContrast` computes and logs the contrast ratios.
It checks body text, code and timestamps against a 4.5:1 minimum, and bold sender names against 3:1.
The background assumptions are `#FFFFFF` (light) and `#1E1E1E` (dark).
These calculations do not guarantee contrast against a different terminal background.

`TestStyle11Golden` compares the exact output for both palettes with `testdata/style11/light.golden`
and `testdata/style11/dark.golden`. The golden files were written before the removal of the other
demo styles (4.3, R3), so the output did not change with that removal.
After an intentional output change, run `go test ./transcript -run TestStyle11Golden -update-style11`
from the parent view directory, and review the golden diff.

The other tests check long lines, hard and soft breaks, tables, links, controls, the gap, and bold cells.
Run `go test -race ./transcript` from the parent view directory.
Physical app appearance remains untested.
The printer's handling of a logical line taller than the pane remains an open limitation.

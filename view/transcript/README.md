# Transcript formatter

Lab prototype. No hub or terminal calls. No pane width.

`Format(Message, previous, Style)` returns a complete message with one blank line after it.
Each message retains its sender and time. The formatter currently ignores `previous`.
Styles are `TextBackground`, `BarBackground`, and `Foreground`.
Styles affect user messages (`Own: true`). Agent bodies use normal text with Markdown emphasis.
The bar appears only at the start of the first body line.
The caller supplies the displayed time in `At`; the formatter does not read the clock.

Goldmark parses Markdown. A tree transformation converts tables into labeled records.
The transformation retains cell inline nodes, including emphasis, code, and links.
Glamour's ANSI renderer then formats the transformed tree without width or padding.
Source soft breaks become spaces. Explicit hard breaks and code lines remain breaks.
The formatter preserves source code indentation. Terminal continuation rows do not repeat it.
Input terminal sequences and control characters are removed, including numeric control entities.
On a renderer error, the formatter uses sanitized literal message text.

The tests check long lines, hard and soft breaks, tables, links, controls, user styles, and a saved ANSI fixture.
Run `go test -race ./transcript` from the parent view directory.
The fixture is `testdata/mixed.ansi`. Physical app appearance remains untested.

## Style gallery

`GalleryStyles` lists eleven `GalleryStyle` values. Each value has a short `String()` name.
`FormatGallery(message, previous, style, dark)` returns one message with a final newline.
It adds a blank line before a new header when a previous message exists.
The caller must reset `previous` when it rebuilds or changes styles.

1. Name chip: white bold sender name on a sender background.
2. Chip and user tint: the same header with a blue user body.
3. Chat markers: a user prompt marker and bold user body; agents have a bullet.
4. Grouped rule: a short rule before each new group, with one header per group.
5. Code prompt: user text has a prompt and a light background; agents have a bullet.
6. Grouped names: colored names and no extra gap between grouped messages.
7. User-only chip: agents have plain colored names.
8. Header marker: a colored bar appears on the header only.
9. Header and user prompt: style 8, with `> ` before each logical user-body line.
10. Header, user prompt, and background: style 9, with a background on user text cells.
11. Header and bold user: style 8 with bold user paragraphs and lists, without a prompt or background.
    User code blocks keep their code color and normal weight. Agent output stays identical to style 8.

Styles 9 and 10 prefix empty logical body lines too. Those prefixes contain one intentional space.
Soft-wrapped continuation rows receive no prefix. Code indentation follows the prefix.
Agent output in styles 9 and 10 is identical to style 8.

The palette uses blue for the user, orange for Claude, green for Codex, and purple for other senders.
Sender matching for colors ignores case. User messages take priority over the author name.
The `dark` argument selects separate light and dark palettes.
Body and code colors are explicit. Chips keep white names on dark sender colors in both palettes.
The printer owns theme detection and passes the selected palette for each message.
Readability in the native app remains unverified.

`TestGalleryContrast` computes and logs the contrast ratios.
It checks body text, code, timestamps, and user prompts against a 4.5:1 minimum.
It checks bold sender names and chip names against a 3:1 minimum.
The default background assumptions are `#FFFFFF` and `#1E1E1E`.
Light body contrast is 17.85:1; dark body contrast is 15.22:1.
The lowest checked body-level ratio is 5.17:1. The lowest name ratio is also 5.17:1.
These calculations do not guarantee contrast against a different terminal background.

Styles 4 and 6 group equal author names and equal `Own` values within five minutes.
The formatter accepts RFC 3339, `Jan 2 · 15:04`, `15:04:05`, and `15:04` timestamps for grouping.
Unknown times, earlier times, and a different sender retain a header.
Short times do not establish dates. The caller must not group separate days under the same short time.
Grouped FYI messages retain a separate `no reply needed` line.

Chips have no padding. Rules have four characters. Body lines have no pane width.
The background style still needs the printer's background-erase handling.
The printer's handling of a logical line taller than the pane remains an open limitation.

Set `GALLERY_FIXTURE=/tmp/gallery.ansi` when running the tests to save the mixed gallery sample.

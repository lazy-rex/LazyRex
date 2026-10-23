package main

// hub.plugins.log {entry} (C7 plan r3): the log of one plugin entry, from a background load
// (pluginloads.go). A load reads two parts:
//   - block: the pane text of the entry's recorded instance (the terminal block method format, with
//     a blockReadMax deadline), the last blockKeep bytes; null when the record has no block;
//   - hub_log: the lines of <state>/hub.log whose message starts with the entry's marker
//     (pluginLogMarker), from the last hubLogTail bytes of the file, at most hubLogLines lines.
//     Only the restart wrapper writes hub.log: a hub without it has no hub.log part to read.
//
// The load reads a snapshot of the entry and its record at its start and at its end. If the two
// differ (another entry, instance or block), the result is stale: the entry goes back to idle, and
// the next request starts a new load.
//
// The block part belongs to the record that the load read (the hub lead, lead-c7cache): its entry
// identity (source and managed entry), instance and block. A kept value of another record is not
// fresh, and the reply never shows its block, in any state; its hub_log part is the entry's
// history, so the reply keeps it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

const (
	logFresh     = 10 * time.Second
	blockReadMax = 5 * time.Second
	blockKeep    = 64 << 10
	hubLogTail   = 256 << 10
	hubLogLines  = 200
)

const noHubLog = "the hub runs without its restart wrapper; it has no hub.log"

// pluginLogMarker is the start of the message of every hub log line about a plugin: "plugin <name>
// [<entry key>]: ". The entry key is opEntryKey. A record with no entry (a kept record) has no key:
// its lines are in no entry's log. An entry with no name (refused before its name was read) has no
// name part.
func pluginLogMarker(name, key string) string {
	switch {
	case key == "":
		return "plugin " + name + ": "
	case name == "":
		return "plugin [" + key + "]: "
	}
	return "plugin " + name + " [" + key + "]: "
}

// pluginLogf writes one hub log line about a plugin. Every plugin line of the hub uses it. A
// message that starts with "plugin <name>: " of the same name (a start error) loses that prefix
// once, so the line does not name the plugin twice; any other text stays.
func pluginLogf(name, key, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if name != "" {
		msg = strings.TrimPrefix(msg, "plugin "+name+": ")
	}
	log.Print(pluginLogMarker(name, key) + msg)
}

// recordLogf is pluginLogf for a record, with the key of its current entry. h.mu must not be held.
func (h *Hub) recordLogf(pl *Plugin, format string, args ...any) {
	h.mu.Lock()
	key := ""
	if pl.entry != nil {
		key = opEntryKey(pl.entry)
	}
	h.mu.Unlock()
	pluginLogf(pl.Name, key, format, args...)
}

// logSnap is what a log load reads of an entry and of the record that it binds.
type logSnap struct {
	name, identity           string
	instance, session, block string
}

// logSnapshot reads the loaded entry with the key, and its record when the entry binds it, under
// entriesMu and then mu (main.go). ok false: no loaded entry has the key.
func (h *Hub) logSnapshot(key string) (s logSnap, ok bool) {
	h.entriesMu.Lock()
	defer h.entriesMu.Unlock()
	for _, e := range h.entries {
		if opEntryKey(e) != key {
			continue
		}
		s.name, s.identity = e.name, entryIdentity(e)
		h.mu.Lock()
		if pl := h.plugins[e.name]; e.name != "" && pl != nil && pl.entry == e {
			s.instance, s.session, s.block = pl.InstanceID, pl.SessionID, pl.BlockID
		}
		h.mu.Unlock()
		return s, true
	}
	return s, false
}

type logBlock struct {
	InstanceID string `json:"instance_id"` // the instance whose pane the text is
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
	Error      string `json:"error,omitempty"`
}

// hubLogPart is the hub_log part. truncated: the reply may not have every line of the entry in
// hub.log (the read began after the start of the file, or more than hubLogLines lines matched).
type hubLogPart struct {
	Available bool     `json:"available"`
	Reason    string   `json:"reason,omitempty"`
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`
	Error     string   `json:"error,omitempty"`
}

type logResult struct {
	snap   logSnap // what the load read: the block part is valid only for this record
	Block  *logBlock
	HubLog hubLogPart
}

// runLogLoad is the load of one entry key.
func (h *Hub) runLogLoad(ctx context.Context, key string) (any, bool, error) {
	before, ok := h.logSnapshot(key)
	if !ok {
		return nil, false, nil
	}
	res := logResult{snap: before}
	if before.block != "" {
		res.Block = h.readBlock(ctx, before)
		res.Block.InstanceID = before.instance
	}
	res.HubLog = readHubLog(h.cfg.hubLog, pluginLogMarker(before.name, key))
	if after, ok := h.logSnapshot(key); !ok || after != before {
		return nil, false, nil
	}
	return res, true, nil
}

// readBlock reads the pane text of the snapshot's block. Rex refuses a reply over 1 MiB
// (too_large, docs/rex-protocol.md), so the hub never receives more than that.
func (h *Hub) readBlock(ctx context.Context, s logSnap) *logBlock {
	call := h.loads.call
	if call == nil {
		call = h.rex.Call
	}
	ctx, cancel := context.WithTimeout(ctx, blockReadMax)
	defer cancel()
	var out struct {
		Content string `json:"content"`
	}
	err := call(ctx, "block.method", map[string]any{"session_id": s.session, "block_id": s.block,
		"method": "format", "args": map[string]any{"format": "text"}}, &out)
	var re *rex.RexError
	switch {
	case errors.As(err, &re) && re.Code == "too_large":
		return &logBlock{Error: "the pane text is over Rex's 1 MiB reply limit"}
	case err != nil:
		return &logBlock{Error: err.Error()}
	}
	text, cut := tailText(out.Content, blockKeep)
	return &logBlock{Text: text, Truncated: cut}
}

// tailText keeps the last max bytes of s. A cut text starts at the first line after the cut, or,
// with no line break in it, at the first full character.
func tailText(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	s = s[len(s)-max:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[i+1:], true
	}
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s, true
}

// Tests only: hubLogRead wraps the reader of the file (a counting reader), and hubLogBeforeRead runs
// between the Stat and the read (a rotation or a truncation).
var (
	hubLogRead       = func(r io.Reader) io.Reader { return r }
	hubLogBeforeRead = func(path string) {}
)

// readHubLog reads the lines of path whose message starts with marker. path "": the hub runs
// without its restart wrapper. An open handle keeps the file that it opened, so a rotation during
// the read does not change the result; a truncation gives a shorter read.
func readHubLog(path, marker string) hubLogPart {
	p := hubLogPart{Available: path != "", Lines: []string{}}
	if path == "" {
		p.Reason = noHubLog
		return p
	}
	f, err := os.Open(path)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		p.Error = err.Error()
		return p
	}
	off := max(st.Size()-hubLogTail, 0)
	hubLogBeforeRead(path)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		p.Error = err.Error()
		return p
	}
	b, err := io.ReadAll(io.LimitReader(hubLogRead(f), hubLogTail))
	if err != nil {
		p.Error = err.Error()
		return p
	}
	text := string(b)
	if off > 0 {
		p.Truncated = true
		_, text, _ = strings.Cut(text, "\n") // the first line can be partial
	}
	for line := range strings.SplitSeq(text, "\n") {
		if logMessageHas(line, marker) {
			p.Lines = append(p.Lines, line)
		}
	}
	if n := len(p.Lines); n > hubLogLines {
		p.Lines, p.Truncated = p.Lines[n-hubLogLines:], true
	}
	return p
}

// logMessageHas reports whether the message of a hub log line (after the log.LstdFlags time
// "2006/01/02 15:04:05 ") starts with marker. A marker inside a message does not count.
func logMessageHas(line, marker string) bool {
	const stamp = "2006/01/02 15:04:05 "
	if len(line) < len(stamp) {
		return false
	}
	if _, err := time.Parse(stamp, line[:len(stamp)]); err != nil {
		return false
	}
	return strings.HasPrefix(line[len(stamp):], marker)
}

// pluginLog is hub.plugins.log {entry, refresh}: entry is a loaded entry's key (opEntryKey).
func (h *Hub) pluginLog(args json.RawMessage) (any, error) {
	var in struct {
		Entry   string `json:"entry"`
		Refresh bool   `json:"refresh"`
	}
	if err := decodeManifest("the arguments", args, &in); err != nil {
		return nil, refuse("invalid_input", "%v", err)
	}
	if in.Entry == "" {
		return nil, refuse("invalid_input", "entry must be the key of a plugin entry")
	}
	cur, ok := h.logSnapshot(in.Entry)
	if !ok {
		return nil, refuse("not_found", "no plugin entry has the key %s", in.Entry)
	}
	stale := func(v any) bool { r, ok := v.(logResult); return ok && r.snap != cur }
	r, err := h.loadRequest(h.loads.logs, in.Entry, in.Refresh, stale)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"entry": in.Entry, "state": r.State, "block": nil, "hub_log": nil, "error": r.Error,
		"read_at": loadTime(r.ReadAt)}
	if v, ok := r.Value.(logResult); ok {
		if v.Block != nil && v.snap == cur {
			out["block"] = v.Block
		}
		out["hub_log"] = v.HubLog
	}
	return out, nil
}

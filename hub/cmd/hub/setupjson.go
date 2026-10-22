package main

// The agent hook files for `lazyrex-hub setup`: ~/.claude/settings.json and ~/.codex/hooks.json.
// Setup adds the hook entries of hooks/README.md and keeps everything else, in its order. A
// map[string]any would sort the keys of the user's file, so these files are read into jnode, an
// ordered JSON value. The output is indented with 2 spaces; the backup keeps the original text.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// jnode is a JSON value that keeps the order of an object's keys.
type jnode struct {
	kind byte     // '{', '[', or 0 for a scalar
	keys []string // an object's keys, in order
	vals []*jnode // an object's values (in key order) or an array's elements
	raw  []byte   // a scalar's JSON text
}

func parseJSON(b []byte) (*jnode, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	n, err := parseNode(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON value")
	}
	return n, nil
}

func parseNode(d *json.Decoder) (*jnode, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t := t.(type) {
	case json.Delim:
		n := &jnode{kind: byte(t)}
		for d.More() {
			if n.kind == '{' {
				k, err := d.Token()
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k.(string))
			}
			v, err := parseNode(d)
			if err != nil {
				return nil, err
			}
			n.vals = append(n.vals, v)
		}
		if _, err := d.Token(); err != nil { // the closing delimiter
			return nil, err
		}
		return n, nil
	case json.Number:
		return &jnode{raw: []byte(t)}, nil
	default:
		return &jnode{raw: jsonText(t)}, nil
	}
}

// jsonText is v's JSON text, with no HTML escaping.
func jsonText(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// get returns the value of an object's last key k, or nil.
func (n *jnode) get(k string) *jnode {
	for i := len(n.keys) - 1; i >= 0; i-- {
		if n.keys[i] == k {
			return n.vals[i]
		}
	}
	return nil
}

func (n *jnode) set(k string, v *jnode) {
	n.keys, n.vals = append(n.keys, k), append(n.vals, v)
}

// str returns a string scalar's value.
func (n *jnode) str() (string, bool) {
	var s string
	if n == nil || n.kind != 0 || json.Unmarshal(n.raw, &s) != nil {
		return "", false
	}
	return s, true
}

func (n *jnode) encode() []byte {
	var b bytes.Buffer
	n.write(&b, "")
	b.WriteByte('\n')
	return b.Bytes()
}

func (n *jnode) write(b *bytes.Buffer, indent string) {
	if n.kind == 0 {
		b.Write(n.raw)
		return
	}
	end := byte('}')
	if n.kind == '[' {
		end = ']'
	}
	b.WriteByte(n.kind)
	if len(n.vals) == 0 {
		b.WriteByte(end)
		return
	}
	in := indent + "  "
	for i, v := range n.vals {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("\n" + in)
		if n.kind == '{' {
			b.Write(jsonText(n.keys[i]))
			b.WriteString(": ")
		}
		v.write(b, in)
	}
	b.WriteString("\n" + indent)
	b.WriteByte(end)
}

// hookGroup is the entry of hooks/README.md: one command hook with "timeout": 5.
func hookGroup(command string) *jnode {
	h := &jnode{kind: '{'}
	h.set("type", &jnode{raw: jsonText("command")})
	h.set("command", &jnode{raw: jsonText(command)})
	h.set("timeout", &jnode{raw: []byte("5")})
	g := &jnode{kind: '{'}
	g.set("hooks", &jnode{kind: '[', vals: []*jnode{h}})
	return g
}

// mergeHook adds the hook entry for command to hooks.<event> in root. It adds nothing, and returns
// a note, when the event already has this command, another lazyrex-hub hook of the same kind, or
// the Python hook (session-start.py): its switch is a separate step (hooks/README.md). Only the
// commands in this event's hook groups count. A key that setup must write into but that has
// another JSON type is an error: setup does not replace it.
func mergeHook(root *jnode, event, kind, command string) (added bool, note string, err error) {
	if root.kind != '{' {
		return false, "", errors.New("the file is not a JSON object")
	}
	hooks := root.get("hooks")
	if hooks == nil {
		hooks = &jnode{kind: '{'}
		root.set("hooks", hooks)
	} else if hooks.kind != '{' {
		return false, "", errors.New(`"hooks" is not an object`)
	}
	list := hooks.get(event)
	if list == nil {
		list = &jnode{kind: '['}
		hooks.set(event, list)
	} else if list.kind != '[' {
		return false, "", fmt.Errorf(`"hooks.%s" is not an array`, event)
	}
	for _, g := range list.vals {
		var hs *jnode
		if g.kind == '{' {
			hs = g.get("hooks")
		}
		if hs == nil || hs.kind != '[' {
			continue
		}
		for _, h := range hs.vals {
			if h.kind != '{' {
				continue
			}
			c, ok := h.get("command").str()
			switch {
			case !ok:
			case c == command:
				return false, "", nil
			case strings.Contains(c, "session-start.py"):
				return false, fmt.Sprintf("hooks.%s has the Python hook (%s). Setup adds no second hook; to switch, see hooks/README.md.", event, c), nil
			case strings.Contains(c, "lazyrex-hub") && strings.HasSuffix(strings.TrimSpace(c), " hook "+kind):
				return false, fmt.Sprintf("hooks.%s has another lazyrex-hub hook (%s). Setup adds no second hook.", event, c), nil
			}
		}
	}
	list.vals = append(list.vals, hookGroup(command))
	return true, "", nil
}

// hookFile proposes the hook entries for an agent's hook file: the events of kind (hooks/README.md).
func hookFile(f *setupFile, kind, command string) {
	if f.block != "" {
		return
	}
	root := &jnode{kind: '{'}
	if f.exists && len(bytes.TrimSpace(f.old)) > 0 {
		var err error
		if root, err = parseJSON(f.old); err != nil {
			f.block = "it is not valid JSON: " + err.Error()
			return
		}
	}
	events := []string{"SessionStart"}
	if kind == "claude" {
		events = append(events, "UserPromptSubmit")
	}
	added := false
	for _, e := range events {
		a, note, err := mergeHook(root, e, kind, command)
		if err != nil {
			f.block = err.Error()
			return
		}
		if note != "" {
			f.notes = append(f.notes, note)
		}
		added = added || a
	}
	if added {
		f.next = root.encode()
	} else {
		f.next = f.old
	}
}

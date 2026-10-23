package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"unicode"
)

type Action struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Enabled bool   `json:"enabled"`
	// ClosesView: the action can close this view (for example archive). The renderer runs it
	// only when every draft is saved, so unsaved text is never lost.
	ClosesView bool `json:"closes_view,omitempty"`
}

type Item struct {
	ID     string  `json:"id"`
	Label  string  `json:"label"`
	Detail string  `json:"detail"`
	Action *Action `json:"action"`
	// Icon is one cell before the label, and Status is short text at the right edge. A list
	// whose items have no Detail shows one row per item.
	Icon   string `json:"icon,omitempty"`
	Status string `json:"status,omitempty"`
}

type Message struct {
	Own    bool   `json:"own,omitempty"`
	ID     string `json:"id"`
	Author string `json:"author"`
	Text   string `json:"text"`
	At     string `json:"at"`
	FYI    bool   `json:"fyi,omitempty"` // the sender expects no reply
}

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// VisibleWhen names a boolean field in the same form. Hidden fields retain their local values.
type FieldCondition struct {
	Field string `json:"field"`
	Value bool   `json:"value"`
}

type Field struct {
	Rev         *int64          `json:"rev,omitempty"`
	Browse      string          `json:"browse,omitempty"`
	ID          string          `json:"id"`
	Label       string          `json:"label"`
	Kind        string          `json:"kind"`
	Value       any             `json:"value"`
	Required    bool            `json:"required"`
	Options     []Option        `json:"options"`
	VisibleWhen *FieldCondition `json:"visible_when,omitempty"`
}

type Node struct {
	ID            string    `json:"id"`
	Type          string    `json:"type"`
	Label         string    `json:"label"`
	Text          string    `json:"text"`
	Items         []Item    `json:"items"`
	Messages      []Message `json:"messages"`
	Before        *string   `json:"before"`
	HasMore       bool      `json:"has_more"`
	PageSize      int       `json:"page_size"`
	Total         *int      `json:"total,omitempty"` // the thread's message count, when sent (v1.1)
	DraftRevision string    `json:"draft_revision"`
	Fields        []Field   `json:"fields"`
	Action        *Action   `json:"action"`
	LoadAction    *Action   `json:"load_action"`
	SaveAction    *Action   `json:"save_action"`
	SendAction    *Action   `json:"send_action"`
	SubmitAction  *Action   `json:"submit_action"`
	RetryAction   *Action   `json:"retry_action"`
}

type Tree struct {
	Title string  `json:"title"`
	Back  *Action `json:"back"`
	// Menu: this form belongs to a menu, so Ctrl+O closes it (Ctrl+O opened the menu).
	Menu  bool   `json:"menu,omitempty"`
	Nodes []Node `json:"nodes"`
}

type Snapshot struct {
	Status        *Status `json:"status,omitempty"`
	SchemaVersion int     `json:"schema_version"`
	ViewID        string  `json:"view_id"`
	Route         string  `json:"route"`
	Revision      string  `json:"revision"`
	Tree          Tree    `json:"tree"`
	// Layout is the hub's effective layout for this view: "transcript" when a transcript pane
	// above prints the history, "" otherwise. A hub without pairs omits it.
	Layout string `json:"layout,omitempty"`
	// Open holds the args of the view's last reopen (thread view v1.1), or nil for none.
	Open *OpenArgs `json:"open,omitempty"`
}

// OpenArgs are the reopen args that the hub keeps for the life of a view's pane. Full: print the
// whole thread, not the newest 500. Any other key is invalid.
type OpenArgs struct {
	Full bool `json:"full"`
}

// UnmarshalJSON accepts only an object whose only key is full, with true or false. encoding/json
// decodes null into a bool without an error, so the value is checked as raw JSON. An absent or
// null open never reaches this method (the pointer stays nil).
func (o *OpenArgs) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		return fmt.Errorf("Invalid open args: not an object")
	}
	args := OpenArgs{}
	for key, value := range raw {
		if key != "full" {
			return fmt.Errorf("Invalid open args: unknown key %q", key)
		}
		switch string(bytes.TrimSpace(value)) {
		case "true":
			args.Full = true
		case "false":
		default:
			return fmt.Errorf("Invalid open args: full is not a boolean")
		}
	}
	*o = args
	return nil
}

func revision(s string) (*big.Int, bool) {
	if s == "" || strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return nil, false
	}
	return new(big.Int).SetString(s, 10)
}

func (s Snapshot) Validate(viewID, route string) error {
	if s.SchemaVersion != 1 {
		return fmt.Errorf("Unsupported view schema %d", s.SchemaVersion)
	}
	if s.ViewID != viewID || s.Route != route {
		return fmt.Errorf("The snapshot belongs to another view")
	}
	if _, ok := revision(s.Revision); !ok {
		return fmt.Errorf("Invalid view revision")
	}
	seen := map[string]bool{"$back": true}
	for _, n := range s.Tree.Nodes {
		if n.ID == "" || seen[n.ID] {
			return fmt.Errorf("Missing or duplicate node ID %q", n.ID)
		}
		seen[n.ID] = true
		if n.Type == "composer" {
			if _, ok := revision(n.DraftRevision); !ok {
				return fmt.Errorf("Invalid draft revision for %s", n.ID)
			}
		}
		if n.Type == "history" && (n.PageSize < 1 || len(n.Messages) > n.PageSize) {
			return fmt.Errorf("Invalid history page for %s", n.ID)
		}
		// Total is checked against this node only, never against what the renderer printed.
		if n.Type == "history" && n.Total != nil && (*n.Total < 0 || *n.Total < len(n.Messages)) {
			return fmt.Errorf("Invalid history total for %s", n.ID)
		}
		ids := map[string]bool{}
		check := func(id string) error {
			if id == "" || ids[id] {
				return fmt.Errorf("Missing or duplicate child ID in %s", n.ID)
			}
			ids[id] = true
			return nil
		}
		for _, i := range n.Items {
			if err := check(i.ID); err != nil {
				return err
			}
		}
		for _, i := range n.Messages {
			if err := check(i.ID); err != nil {
				return err
			}
		}
		for _, f := range n.Fields {
			if err := check(f.ID); err != nil {
				return err
			}
			if f.VisibleWhen != nil {
				found := false
				for _, other := range n.Fields {
					if other.ID == f.VisibleWhen.Field && other.Kind == "boolean" && other.ID != f.ID {
						found = true
					}
				}
				if !found {
					return fmt.Errorf("Invalid visibility condition for %s", f.ID)
				}
			}
			switch f.Kind {
			case "text", "multiline", "boolean", "select":
			default:
				return fmt.Errorf("Unsupported form field %q", f.Kind)
			}
		}
	}
	return nil
}

// plain keeps terminal control bytes out of plugin text.
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

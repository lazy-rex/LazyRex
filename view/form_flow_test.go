package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func flowSnapshot() Snapshot {
	return Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/forms/start", Revision: "1", Tree: Tree{Nodes: []Node{
		{ID: "start", Type: "form", Fields: []Field{
			{ID: "dir", Label: "Directory", Kind: "text", Browse: "native"},
			{ID: "profile", Label: "Profile", Kind: "select", Value: "a", Options: []Option{{Value: "a", Label: "Shell"}}},
			{ID: "title", Label: "Tab title", Kind: "text"},
		}, SubmitAction: &Action{Name: "submit", Label: "Start", Enabled: true}},
		{ID: "cancel", Type: "widget", Action: &Action{Name: "$cancel", Label: "Cancel", Enabled: true}},
	}}}
}

// Submit and a plain Cancel button share one row, and each has its own hit area.
func TestSubmitAndCancelShareARow(t *testing.T) {
	m := testModel()
	m.apply(flowSnapshot())
	for _, r := range m.bodyRows() {
		cs := r.rowControls()
		if len(cs) == 2 && cs[0].ID == "start:submit" && cs[1].ID == "cancel:action" {
			if hit, ok := m.hitRows([]row{r}, m.leftMargin()+cs[1].Left, 0); !ok || hit.ID != "cancel:action" {
				t.Fatalf("Cancel has no hit area: %+v", hit)
			}
			if hit, ok := m.hitRows([]row{r}, m.leftMargin(), 0); !ok || hit.ID != "start:submit" {
				t.Fatalf("Submit has no hit area: %+v", hit)
			}
			ids := []string{}
			for _, c := range m.controls() {
				ids = append(ids, c.ID)
			}
			if got := strings.Join(ids, ","); !strings.HasSuffix(got, "start:submit,cancel:action") {
				t.Fatalf("Tab order does not end in Submit, Cancel: %s", got)
			}
			return
		}
	}
	t.Fatal("Submit and Cancel are not on one row")
}

// Browse… shares the rows of its text box, and the box leaves room for it.
func TestBrowseBesideTheTextBox(t *testing.T) {
	m := testModel()
	m.apply(flowSnapshot())
	for _, r := range m.bodyRows() {
		cs := r.rowControls()
		if len(cs) == 2 && cs[0].Kind == "edit" && cs[1].Kind == "browse" {
			if cs[1].Left+cs[1].HitWidth > m.controlWidth() {
				t.Fatalf("Browse ends past the field width: %+v", cs[1])
			}
			if ansi.StringWidth(r.Text) > m.contentWidth() {
				t.Fatal("The row exceeds the content width")
			}
			return
		}
	}
	t.Fatal("Browse is not beside its text box")
}

// A select gets the same gap as a text field; the footer names no history in a form.
func TestFormSpacingAndFooter(t *testing.T) {
	m := testModel()
	m.themeReady = true
	m.apply(flowSnapshot())
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	for i, l := range lines {
		if strings.Contains(l, "Shell ▾") {
			if i+2 >= len(lines) || strings.TrimSpace(lines[i+1]) == "" && strings.TrimSpace(lines[i+2]) != "" && !strings.Contains(lines[i+2], "Tab title") {
				t.Fatalf("Unexpected rows after the select:\n%s", strings.Join(lines, "\n"))
			}
		}
		if strings.Contains(l, "Tab title") && i > 0 && strings.TrimSpace(lines[i-1]) != "" {
			t.Fatalf("No gap before the field after a select:\n%s", strings.Join(lines, "\n"))
		}
	}
	if out := strings.Join(lines, "\n"); strings.Contains(out, "PgUp/PgDn") {
		t.Fatalf("A form footer names history:\n%s", out)
	}
}

// Items without details show one row each, with the status at the right.
func TestCompactListRows(t *testing.T) {
	m := testModel()
	s := testSnapshot("", "1")
	s.Tree.Nodes = []Node{{ID: "agents", Type: "list", Label: "Agents", Items: []Item{
		{ID: "a", Label: "claude-7", Icon: "●", Status: "working", Action: &Action{Name: "agent", Enabled: true}},
		{ID: "b", Label: "codex-2", Icon: "◌", Status: "no tab"},
	}}}
	m.apply(s)
	m.themeReady = true
	out := ansi.Strip(m.View().Content)
	if !strings.Contains(out, "AGENTS") || !strings.Contains(out, "working ›") || strings.Contains(out, "no tab ›") {
		t.Fatalf("Compact list rows are wrong:\n%s", out)
	}
	if c, ok := m.hitControl(m.leftMargin()+3, headerRows+1); !ok || c.ID != "agents:a" {
		t.Fatalf("Row 1 target %q, want agents:a", c.ID)
	}
	if c, ok := m.hitControl(m.leftMargin()+3, headerRows+2); ok {
		t.Fatalf("A disabled row has target %q", c.ID)
	}
}

// Ctrl+O closes a menu's form; in other forms it does nothing.
func TestCtrlOClosesAMenuForm(t *testing.T) {
	for _, menu := range []bool{true, false} {
		m := testModel()
		s := flowSnapshot()
		s.Tree.Menu = menu
		m.apply(s)
		m.themeReady = true
		if got := strings.Contains(ansi.Strip(m.View().Content), "Ctrl+O close"); got != menu {
			t.Fatalf("menu=%v: footer names Ctrl+O close: %v", menu, got)
		}
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
		if (cmd != nil) != menu {
			t.Fatalf("menu=%v: Ctrl+O command %v", menu, cmd != nil)
		}
	}
}

// Tab visits every control in screen order (rows top to bottom, then left to right inside a row),
// and wraps at both ends. Shift+Tab visits them in reverse. This replaces the key-press checks of
// the plugin Node lab tests (Tab to a button, Enter on the focused item).
func TestTabVisitsControlsInScreenOrder(t *testing.T) {
	list := Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/panels/x", Revision: "1", Tree: Tree{Nodes: []Node{
		{ID: "items", Type: "list", Items: []Item{
			{ID: "a", Label: "First", Action: &Action{Name: "open", Enabled: true}},
			{ID: "b", Label: "Second", Action: &Action{Name: "open", Enabled: true}},
		}},
		{ID: "stop", Type: "widget", Text: "Forward 3000", Action: &Action{Name: "stop", Label: "Stop", Enabled: true}},
	}}}
	cases := map[string]struct {
		snap Snapshot
		want []string
	}{
		"form": {flowSnapshot(), []string{"edit:start/dir", "browse:start/dir", "choice:start/profile", "edit:start/title", "start:submit", "cancel:action"}},
		"list": {list, []string{"items:a", "items:b", "stop:action"}},
	}
	for name, tc := range cases {
		m := testModel()
		m.apply(tc.snap)
		var screen []string
		seen := map[string]bool{}
		for _, r := range m.bodyRows() {
			for _, c := range r.rowControls() {
				if !seen[c.ID] { // a box spans several rows
					seen[c.ID] = true
					screen = append(screen, c.ID)
				}
			}
		}
		var order []string
		for _, c := range m.controls() {
			order = append(order, c.ID)
		}
		if strings.Join(screen, ",") != strings.Join(tc.want, ",") || strings.Join(order, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s: screen order %v, Tab order %v, want %v", name, screen, order, tc.want)
		}
		order = tc.want
		m.setFocus(order[0])
		for i := 1; i <= len(order); i++ {
			m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			if want := order[i%len(order)]; m.focus != want {
				t.Fatalf("%s: Tab %d focused %q, want %q", name, i, m.focus, want)
			}
		}
		for i := len(order) - 1; i >= 0; i-- {
			m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
			if m.focus != order[i] {
				t.Fatalf("%s: Shift+Tab focused %q, want %q", name, m.focus, order[i])
			}
		}
	}
}

// Enter activates the focused control: the second list item, and a widget button reached by Tab.
func TestEnterActivatesTheFocusedControl(t *testing.T) {
	m := testModel()
	m.apply(Snapshot{SchemaVersion: 1, ViewID: "v1", Route: "/panels/x", Revision: "1", Tree: Tree{Nodes: []Node{
		{ID: "items", Type: "list", Items: []Item{
			{ID: "a", Label: "First", Action: &Action{Name: "open", Enabled: true}},
			{ID: "b", Label: "Second", Action: &Action{Name: "open", Enabled: true}},
		}},
		{ID: "stop", Type: "widget", Text: "Forward 3000", Action: &Action{Name: "stop", Label: "Stop", Enabled: true}},
	}}})
	cs := m.controls()
	m.setFocus(cs[0].ID)
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if req := request(t, m, cmd); req.Node != "items" || req.Args["action"] != "open" || req.Args["input"].(map[string]any)["item_id"] != "b" {
		t.Fatalf("Enter on the second item sent %+v", req)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if req := request(t, m, cmd); req.Node != "stop" || req.Args["action"] != "stop" {
		t.Fatalf("Enter on Stop sent %+v", req)
	}
}

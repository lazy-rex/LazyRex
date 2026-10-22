package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// TestControlGallery renders the production controls without a Rex connection.
func TestControlGallery(t *testing.T) {
	dir := os.Getenv("LAZYREX_GALLERY")
	if dir == "" {
		t.Skip("Set LAZYREX_GALLERY to export the control gallery")
	}
	for _, width := range []int{38, 100} {
		for _, dark := range []bool{false, true} {
			for _, kind := range []string{"yesno", "list", "button", "input", "select", "composer"} {
				var lines []string
				lines = append(lines, kind+" controls", strings.Repeat("─", width-6), "")
				for _, state := range []string{"Normal", "Hover", "Focus", "Pressed", "Selected", "Disabled", "Editor focus"} {
					if state == "Editor focus" && kind != "composer" {
						continue
					}
					if kind == "list" && state == "Selected" {
						continue
					}
					if (kind == "yesno" || kind == "select") && state == "Disabled" {
						continue
					} // The schema does not define disabled fields.
					if kind == "input" && (state == "Disabled" || state == "Pressed" || state == "Selected") {
						continue
					}
					if kind == "button" && state == "Selected" {
						continue
					}
					if kind == "composer" && state == "Selected" {
						continue
					}
					m := testModel()
					m.width = width
					m.height = 60
					m.dark = dark
					s := testSnapshot("A draft ready to send", "1")
					s.Tree.Nodes = nil
					id := ""
					switch kind {
					case "yesno":
						s.Tree.Nodes = []Node{{ID: "f", Type: "form", Fields: []Field{{ID: "choice", Kind: "boolean", Label: "Include Claude", Value: state == "Selected"}}}}
						id = "yes:f/choice"
					case "list":
						s.Tree.Nodes = []Node{{ID: "list", Type: "list", Items: []Item{{ID: "a", Label: "Design review", Detail: "2 unread messages", Action: &Action{Name: "open", Enabled: state != "Disabled"}}}}}
						id = "list:a"
					case "button":
						s.Tree.Nodes = []Node{{ID: "f", Type: "form", SubmitAction: &Action{Name: "create", Label: "Create thread", Enabled: state != "Disabled"}}}
						id = "f:submit"
					case "select":
						value := "low"
						if state == "Selected" {
							value = "high"
						}
						s.Tree.Nodes = []Node{{ID: "f", Type: "form", Fields: []Field{{ID: "priority", Kind: "select", Label: "Priority", Value: value, Options: []Option{{Value: "low", Label: "Normal"}, {Value: "high", Label: "High"}}}}}}
						id = "choice:f/priority"
					case "input":
						s.Tree.Nodes = []Node{{ID: "f", Type: "form", Fields: []Field{{ID: "title", Kind: "text", Label: "Thread title", Value: "Design review"}}}}
						id = "edit:f/title"
					case "composer":
						s = testSnapshot("A draft ready to send", "1")
						id = "composer:send"
						if state == "Disabled" {
							s = testSnapshot("", "1")
						}
					}
					m.apply(s)
					m.setFocus("")
					m.hover = ""
					m.pressed = ""
					if state == "Hover" || state == "Pressed" {
						m.hover = id
					}
					if state == "Focus" {
						m.setFocus(id)
					}
					if state == "Pressed" {
						m.pressed = id
					}
					if state == "Editor focus" {
						m.setFocus("edit:composer")
					}
					label := state
					if kind == "composer" && state == "Disabled" {
						label = "Empty draft · Send unavailable"
					}
					if kind == "composer" && state == "Focus" {
						label = "Send keyboard focus"
					}
					lines = append(lines, lipgloss.NewStyle().Foreground(m.palette().muted).Render(label))
					rows := m.rows()
					if kind == "button" && state == "Disabled" {
						lines = append(lines, "Disabled actions are hidden by this schema.")
					}
					for _, r := range rows {
						lines = append(lines, r.Text)
					}
					lines = append(lines, "")
				}
				for i := range lines {
					lines[i] = strings.Repeat(" ", 3) + lines[i]
				}
				name := fmt.Sprintf("%dx%d-dark-%t-%s.ansi", width, len(lines), dark, kind)
				if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestMessagesGallery(t *testing.T) {
	dir := os.Getenv("LAZYREX_GALLERY")
	if dir == "" {
		t.Skip("Set LAZYREX_GALLERY to export")
	}
	for _, width := range []int{38, 100} {
		for _, dark := range []bool{false, true} {
			m := testModel()
			m.width = width
			m.height = 40
			m.dark = dark
			m.status = "Connected"
			m.ready = true
			s := testSnapshot("", "1")
			s.Tree.Title = "Messages"
			s.Tree.Nodes = []Node{
				{ID: "threads", Type: "list", Label: "Conversations", Items: []Item{{ID: "claude", Label: "Claude", Detail: "No unread messages", Action: &Action{Name: "open", Enabled: true}}, {ID: "codex", Label: "Codex", Detail: "No unread messages", Action: &Action{Name: "open", Enabled: true}}, {ID: "team", Label: "Team", Detail: "2 unread messages", Action: &Action{Name: "open", Enabled: true}}}},
				{ID: "new", Type: "form", Label: "New thread", Fields: []Field{{ID: "title", Kind: "text", Label: "Title (optional)", Value: ""}, {ID: "claude", Kind: "boolean", Label: "Claude", Value: false}, {ID: "codex", Kind: "boolean", Label: "Codex", Value: true}}, SubmitAction: &Action{Name: "create", Label: "Create thread", Enabled: true}},
			}
			m.apply(s)
			m.setFocus("yes:new/claude")
			m.hover = "threads:claude"
			name := fmt.Sprintf("%dx40-dark-%t-messages.ansi", width, dark)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(m.View().Content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

package main

import (
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

func TestFormWaitsForInitialTheme(t *testing.T) {
	for _, reply := range []tea.Msg{
		uv.DarkColorSchemeEvent{}, uv.LightColorSchemeEvent{},
		tea.BackgroundColorMsg{Color: color.Black}, tea.BackgroundColorMsg{Color: color.White},
	} {
		m := newModel(&viewClient{Route: "/forms/new-thread"})
		if got := m.View(); got.Content != "" || !got.AltScreen {
			t.Fatal("form drew before the first theme reply")
		}
		m.Update(reply)
		if !m.themeReady || m.View().Content == "" {
			t.Fatalf("%T did not release the first draw", reply)
		}
		m.Update(formThemeTimeout{})
		if m.View().Content == "" {
			t.Fatal("late timeout hid the form")
		}
	}
}

func TestFormDrawsAfterThemeTimeout(t *testing.T) {
	m := newModel(&viewClient{Route: "/forms/new-thread"})
	m.Update(formThemeTimeout{})
	if m.View().Content == "" {
		t.Fatal("missing theme reply left the form blank")
	}
	m.Update(tea.BackgroundColorMsg{Color: color.Black})
	if !m.dark || m.View().Content == "" {
		t.Fatal("late theme reply did not update the visible form")
	}
	thread := testModel()
	if thread.View().Content == "" {
		t.Fatal("ordinary thread waited for the form theme timeout")
	}
}

// The huh theme takes every color from a palette role, in the renderer's theme, whatever huh
// passes as isDark.
func TestHuhThemeUsesPaletteRoles(t *testing.T) {
	for _, dark := range []bool{false, true} {
		m := testModel()
		m.dark = dark
		p := m.palette()
		for _, isDark := range []bool{false, true} {
			s := m.huhTheme().Theme(isDark)
			for name, got := range map[string][2]color.Color{
				"title":           {s.Focused.Title.GetForeground(), p.title},
				"selector":        {s.Focused.SelectSelector.GetForeground(), p.highlight},
				"selected":        {s.Focused.SelectedOption.GetForeground(), p.selected},
				"option":          {s.Focused.UnselectedOption.GetForeground(), p.text},
				"error":           {s.Focused.ErrorIndicator.GetForeground(), p.danger},
				"focused button":  {s.Focused.FocusedButton.GetBackground(), p.highlight},
				"focused label":   {s.Focused.FocusedButton.GetForeground(), p.onHighlight},
				"blurred button":  {s.Focused.BlurredButton.GetBackground(), p.button},
				"blurred label":   {s.Blurred.BlurredButton.GetForeground(), p.text},
				"field border":    {s.Focused.Base.GetBorderLeftForeground(), p.control},
				"blurred title":   {s.Blurred.Title.GetForeground(), p.title},
				"input prompt":    {s.Focused.TextInput.Prompt.GetForeground(), p.highlight},
				"placeholder":     {s.Focused.TextInput.Placeholder.GetForeground(), p.muted},
				"description":     {s.Focused.Description.GetForeground(), p.muted},
				"selected prefix": {s.Focused.SelectedPrefix.GetForeground(), p.selected},
			} {
				if got[0] != got[1] {
					t.Fatalf("dark=%t isDark=%t: %s is %v, want the role %v", dark, isDark, name, got[0], got[1])
				}
			}
			for _, b := range []lipgloss.Style{s.Focused.FocusedButton, s.Focused.BlurredButton} {
				if top, right, bottom, left := b.GetPadding(); top != 0 || bottom != 0 || left != buttonPadding || right != buttonPadding {
					t.Fatalf("Button padding %d %d %d %d, want 0 %d 0 %d", top, right, bottom, left, buttonPadding, buttonPadding)
				}
			}
		}
	}
}

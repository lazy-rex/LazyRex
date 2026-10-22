package main

import (
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// huhTheme gives huh fields the renderer's palette roles. It ignores huh's isDark argument: a
// standalone field learns the background only from a tea.BackgroundColorMsg, and the renderer
// knows the theme from its own OSC 11 and mode 2031 reports. The shapes come from huh's
// ThemeBase; every color comes from a palette role.
func (m *model) huhTheme() huh.Theme {
	return huh.ThemeFunc(func(bool) *huh.Styles { return m.huhStyles() })
}

func (m *model) huhStyles() *huh.Styles {
	p := m.palette()
	t := huh.ThemeBase(m.dark)
	button := lipgloss.NewStyle().Padding(0, buttonPadding).MarginRight(1)

	f := &t.Focused
	f.Base = f.Base.BorderForeground(p.control)
	f.Card = f.Base
	f.Title = f.Title.Foreground(p.title).Bold(true)
	f.NoteTitle = f.NoteTitle.Foreground(p.title).Bold(true).MarginBottom(1)
	f.Directory = f.Directory.Foreground(p.title)
	f.Description = f.Description.Foreground(p.muted)
	f.ErrorIndicator = f.ErrorIndicator.Foreground(p.danger)
	f.ErrorMessage = f.ErrorMessage.Foreground(p.danger)
	f.SelectSelector = f.SelectSelector.Foreground(p.highlight)
	f.NextIndicator = f.NextIndicator.Foreground(p.highlight)
	f.PrevIndicator = f.PrevIndicator.Foreground(p.highlight)
	f.Option = f.Option.Foreground(p.text)
	f.MultiSelectSelector = f.MultiSelectSelector.Foreground(p.highlight)
	f.SelectedOption = f.SelectedOption.Foreground(p.selected)
	f.SelectedPrefix = lipgloss.NewStyle().Foreground(p.selected).SetString("✓ ")
	f.UnselectedPrefix = lipgloss.NewStyle().Foreground(p.muted).SetString("• ")
	f.UnselectedOption = f.UnselectedOption.Foreground(p.text)
	f.FocusedButton = button.Foreground(p.onHighlight).Background(p.highlight).Bold(true)
	f.Next = f.FocusedButton
	f.BlurredButton = button.Foreground(p.text).Background(p.button)
	f.TextInput.Cursor = f.TextInput.Cursor.Foreground(p.accent)
	f.TextInput.Placeholder = f.TextInput.Placeholder.Foreground(p.muted)
	f.TextInput.Prompt = f.TextInput.Prompt.Foreground(p.highlight)
	f.TextInput.Text = f.TextInput.Text.Foreground(p.text)

	// A blurred field has the same colors, a hidden border and no option indicators (ThemeCharm).
	t.Blurred = t.Focused
	t.Blurred.Base = t.Focused.Base.BorderStyle(lipgloss.HiddenBorder())
	t.Blurred.Card = t.Blurred.Base
	t.Blurred.NextIndicator = lipgloss.NewStyle()
	t.Blurred.PrevIndicator = lipgloss.NewStyle()

	t.Group.Title = f.Title
	t.Group.Description = f.Description
	return t
}

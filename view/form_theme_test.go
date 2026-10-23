package main

import (
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

func TestFormWaitsForInitialTheme(t *testing.T) {
	for _, reply := range []tea.Msg{
		uv.DarkColorSchemeEvent{}, uv.LightColorSchemeEvent{},
		tea.BackgroundColorMsg{Color: color.Black}, tea.BackgroundColorMsg{Color: color.White},
	} {
		m := newModel(&viewClient{Route: "/forms/new-thread"})
		// No route decides the mode (thread view v1.2): before its first tree a form is neutral, so the
		// empty frame is inline.
		if got := m.View(); got.Content != "" || got.AltScreen {
			t.Fatal("form drew before the first theme reply, or turned the alt screen on before its tree")
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

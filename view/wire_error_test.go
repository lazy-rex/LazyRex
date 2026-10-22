package main

import "testing"

// A hub refusal message that already starts with its own code shows the code once. Any other
// message keeps the code before it, also a message that starts with another code.
func TestWireErrorCodeOnce(t *testing.T) {
	for _, c := range []struct{ code, message, want string }{
		{"no_app", "no_app: no Rex app shows this session", "no_app: no Rex app shows this session"},
		{"no_app", "No app is connected", "no_app: No app is connected"},
		{"refused", "no_app: no Rex app shows this session", "refused: no_app: no Rex app shows this session"},
		{"no_app", "no_app:no space", "no_app: no_app:no space"},
		{"no_app", "", "no_app: "},
	} {
		if got := (&wireError{Code: c.code, Message: c.message}).Error(); got != c.want {
			t.Errorf("wireError{%q, %q}.Error() = %q, want %q", c.code, c.message, got, c.want)
		}
	}
}

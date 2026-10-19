package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMultiAssignHazard(t *testing.T) {
	bad := []string{
		"a, b = b, a",
		"  if b < a then a, b = b, a end",
		"x, y = y, x + 10",
		"x, y = f(), g(x)",
	}
	good := []string{
		"local a, b = b, a",
		"t[1], t[2] = t[2], t[1]",
		"a, b = canonical(a), canonical(b)",
		"value.intents, value.forwards = value.intents or {}, value.forwards or {}",
		"if a == b then return end",
		"local t = a; a = b; b = t",
		"f(a, b) -- a, b = b, a",
		"for k, v in pairs(t) do x = k end",
		"id, carry = tostring(n % 10) .. id, math.floor(n / 10)",
	}
	for _, l := range bad {
		if !multiAssignHazard(l) {
			t.Errorf("not reported: %s", l)
		}
	}
	for _, l := range good {
		if multiAssignHazard(l) {
			t.Errorf("false report: %s", l)
		}
	}
}

func TestLuaWarningsNameFileAndLine(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "p.lua"), []byte("local x = 1\nlocal y = 2\nx, y = y, x\n"), 0o600)
	w := luaWarnings(dir)
	if len(w) != 1 || !strings.Contains(w[0], "p.lua:3:") {
		t.Fatalf("warnings: %v", w)
	}
}

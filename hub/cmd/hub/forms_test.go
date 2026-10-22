package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseCommands(t *testing.T) {
	ok := []any{map[string]any{"id": "new_thread", "title": "New Thread…", "route": "/forms/new-thread", "keywords": []any{"message"}}}
	cmds, err := parseCommands("messaging", ok)
	if err != nil || len(cmds) != 1 || cmds[0].Keywords[0] != "message" {
		t.Fatalf("valid command: %v %v", cmds, err)
	}
	if cmds, err := parseCommands("messaging", map[string]any{}); err != nil || cmds != nil {
		t.Fatalf("{} (an empty Lua table) is no commands: %v %v", cmds, err)
	}
	for name, bad := range map[string]any{
		"id with a dot":   []any{map[string]any{"id": "a.b", "title": "T", "route": "/forms/x"}},
		"no title":        []any{map[string]any{"id": "a", "title": " ", "route": "/forms/x"}},
		"not a form":      []any{map[string]any{"id": "a", "title": "T", "route": "/thread/x"}},
		"duplicate id":    []any{map[string]any{"id": "a", "title": "T", "route": "/forms/x"}, map[string]any{"id": "a", "title": "U", "route": "/forms/y"}},
		"not a list":      "x",
		"uppercase id":    []any{map[string]any{"id": "New", "title": "T", "route": "/forms/x"}},
		"lua-unsafe name": nil,
	} {
		plugin := "messaging"
		if name == "lua-unsafe name" {
			plugin, bad = "my-plugin", ok
		}
		if _, err := parseCommands(plugin, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPaletteLua(t *testing.T) {
	cmds := map[string][]paletteCommand{
		"worktrees": {{ID: "new", Title: "New Worktree…", Route: "/forms/new"}},
		"messaging": {{ID: "new_thread", Title: `Say "hi"` + "\n\\", Description: "Start a thread", Keywords: []string{"message", "chat"}, Route: "/forms/new-thread"}},
	}
	a, b := paletteLua(cmds), paletteLua(cmds)
	if a != b {
		t.Fatal("the same commands must give the same bytes")
	}
	for _, want := range []string{
		`name = "lazyrex_messaging_new_thread",`,
		`title = "LazyRex: Say \"hi\"\010\\",`,
		`keywords = "message chat",`,
		`run = lazyrex_run("messaging.new_thread"),`,
		`name = "lazyrex_worktrees_new",`,
	} {
		if !strings.Contains(a, want) {
			t.Errorf("missing %s in\n%s", want, a)
		}
	}
	if strings.Index(a, "lazyrex_messaging_new_thread") > strings.Index(a, "lazyrex_worktrees_new") {
		t.Error("plugins are sorted")
	}
	// The hub's own entries, also without any plugin command.
	for _, want := range []string{
		`run = lazyrex_admin("hub.admin.resume", "resume", {}),`,
		`run = lazyrex_admin("hub.admin.restart", "restart-codex", { kind = "codex" }),`,
		`run = lazyrex_admin("hub.admin.restart", "restart-claude", { kind = "claude" }),`,
	} {
		if !strings.Contains(paletteLua(nil), want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestCleanTitle(t *testing.T) {
	for in, want := range map[string]string{
		"✳ Fix transparent form":           "Fix transparent form",
		"⠂ Codex spike":                    "Codex spike",
		"Codex spike architecture via rex": "Codex spike architecture via rex",
		"  ✳  ":                            "",
		"":                                 "",
		"2 builds":                         "2 builds",
		".env":                             ".env",
		"~/project":                        "~/project",
		"🔥 hot fix":                        "hot fix",
		"→ next":                           "next",
	} {
		if got := cleanTitle(in); got != want {
			t.Errorf("cleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestViewArgsCarryViewID(t *testing.T) {
	a := viewArgs("view-form-1", "/forms/start", `{"session_id":"s"}`)
	if a["view_id"] != "view-form-1" || a["route"] != "/forms/start" || a["user"] != viewUser || string(a["context"].(json.RawMessage)) != `{"session_id":"s"}` {
		t.Fatalf("form args: %v", a)
	}
	if b := viewArgs("view-tab", "/", ""); b["view_id"] != "view-tab" || b["context"] != nil {
		t.Fatalf("tab args: %v", b)
	}
}

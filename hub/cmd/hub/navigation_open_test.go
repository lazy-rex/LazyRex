package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestExplicitNavigationOpensUndeclaredTabOnce(t *testing.T) {
	for _, scenario := range []string{"declares", "lost answer", "unsupported", "still missing", "already declared"} {
		t.Run(scenario, func(t *testing.T) {
			h := testUIHub(t)
			ctx := context.Background()
			calls := 0
			declare := func() {
				t.Helper()
				_, err := h.uiTabs(ctx, "messaging", map[string]any{"tabs": []any{map[string]any{"key": "thread/a", "title": "A", "route": "/thread/a"}}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "already declared" {
				declare()
			}
			open := func(_ context.Context, plugin, action string, args any, out any) error {
				calls++
				if plugin != "messaging" || action != "view.open" || !reflect.DeepEqual(args, map[string]any{"route": "/thread/a", "user": viewUser}) {
					t.Fatalf("Unexpected open: %s %s %#v", plugin, action, args)
				}
				if scenario == "declares" || scenario == "lost answer" {
					declare()
				}
				if scenario == "unsupported" || scenario == "lost answer" {
					return errors.New(scenario)
				}
				return nil
			}
			err := h.navigationTab(ctx, "messaging", "thread/a", open)
			if scenario == "unsupported" || scenario == "still missing" {
				if err == nil || !strings.Contains(err.Error(), `has no tab "thread/a"`) {
					t.Fatalf("Missing tab result: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			expected := 1
			if scenario == "already declared" {
				expected = 0
			}
			if calls != expected {
				t.Fatalf("Open calls %d, want %d", calls, expected)
			}
		})
	}
}

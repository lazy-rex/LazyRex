package main

import (
	"context"
	"strings"
	"testing"
)

func TestStartBefore(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"1.000001", "1.000002", true}, {"1.000002", "1.000001", false}, {"9.999999", "10.000000", true}, {"5.000000", "5.000000", false}} {
		if got := startBefore(c.a, c.b); got != c.want {
			t.Errorf("startBefore(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestStoreScanPagesOneNamespace(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	for _, r := range [][2]string{{"a", "mail/L/1"}, {"a", "mail/L/2"}, {"a", "mail/L/3"}, {"a", "mailx"}, {"b", "mail/L/9"}} {
		if _, err := h.db.Exec(`INSERT INTO records VALUES (?, ?, '1', 1)`, r[0], r[1]); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{}
	after := ""
	for page := 0; page < 5; page++ {
		out, err := h.db.storeScan(ctx, "a", map[string]any{"prefix": "mail/", "after": after, "limit": float64(2)})
		if err != nil {
			t.Fatal(err)
		}
		m := out.(map[string]any)
		for k := range m["records"].(map[string]any) {
			got[k] = true
		}
		next, _ := m["next"].(string)
		if next == "" {
			break
		}
		after = next
	}
	if len(got) != 3 || !got["mail/L/1"] || !got["mail/L/3"] || got["mailx"] || got["mail/L/9"] {
		t.Fatalf("scan = %v", got)
	}
}

// A new identity record inherits the delivery suppression of the legacy agent in its pane, so the
// alias redirect does not write the same notice to the same pane twice.
func TestIdentitySyncInheritsLegacyDelivery(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	if _, err := h.db.Exec(identitySchema); err != nil {
		t.Fatal(err)
	}
	h.agents["claude"] = &Agent{ID: "claude", BlockID: "block:1", delivered: map[string]int64{"messaging": 7}}
	h.db.Exec(`INSERT INTO identity_panes VALUES ('block:1', 'session:1', 0, '', 'claude', 'claude:c1', '')`)
	h.db.Exec(`INSERT INTO identities VALUES ('claude:c1', 'claude', 'c1', 'current', 'block:1', '', '', 'x', 'x')`)
	h.db.Exec(`INSERT INTO identity_aliases VALUES ('claude', 'claude:c1', 'x')`)
	h.idents = map[string]*Agent{}
	if err := h.identitySync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.idents["claude:c1"].delivered["messaging"]; got != 7 {
		t.Fatalf("delivered = %d, want 7", got)
	}
	h.agents["claude"].delivered["messaging"] = 9 // a later legacy write must not leak into a copy
	if h.idents["claude:c1"].delivered["messaging"] != 7 {
		t.Fatal("the identity shares the legacy map")
	}
}

// Another conversation in the same pane (after /clear) does not inherit the legacy delivery state.
func TestIdentitySyncNoDeliveryForOtherConversation(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	if _, err := h.db.Exec(identitySchema); err != nil {
		t.Fatal(err)
	}
	h.agents["claude"] = &Agent{ID: "claude", BlockID: "block:1", StatusRecord: "rec", delivered: map[string]int64{"messaging": 7}}
	h.db.Exec(`INSERT INTO identity_panes VALUES ('block:1', 'session:1', 0, '', 'claude', 'claude:c2', '')`)
	h.db.Exec(`INSERT INTO identities VALUES ('claude:c1', 'claude', 'c1', 'superseded', 'block:1', '', '', 'x', 'x')`)
	h.db.Exec(`INSERT INTO identities VALUES ('claude:c2', 'claude', 'c2', 'current', 'block:1', 'claude:c1', '', 'x', 'x')`)
	h.db.Exec(`INSERT INTO identity_aliases VALUES ('claude', 'claude:c1', 'x')`)
	h.idents = map[string]*Agent{}
	if err := h.identitySync(ctx); err != nil {
		t.Fatal(err)
	}
	c2 := h.idents["claude:c2"]
	if c2.delivered["messaging"] != 0 {
		t.Fatalf("claude:c2 inherited delivery %d from an alias it does not own", c2.delivered["messaging"])
	}
	if c2.StatusRecord != "rec" {
		t.Fatalf("status record = %q, want the pane's rec", c2.StatusRecord)
	}
}

// Identity reports come from the agent hooks only: the MCP shim, the one sender of source "mcp",
// is gone. The hub refuses that source before it looks for the pane.
func TestIdentityReportRefusesSourceMCP(t *testing.T) {
	h, _ := workerTestHub(t)
	_, err := h.identityReport(context.Background(), perform{}, map[string]any{"kind": "codex", "conversation_id": "thread-1", "source": "mcp"})
	if err == nil || !strings.Contains(err.Error(), "invalid: source must be one of startup, resume, clear, compact, fork, prompt") {
		t.Fatalf("source mcp: %v", err)
	}
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// A scan page ends by encoded size, not only by count. Records full of '<' (6 bytes each when
// encoded) would make one default page of 500 records about 12 MB, far over the reply limit.
// Each page stays under scanPageBytes plus the envelope, has at least one record, and the pages
// hold every key once, in key order.
func TestStoreScanPageEndsBySize(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	value := `"` + strings.Repeat("<", 4000) + `"`
	var want []string
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("thread/%04d", i)
		want = append(want, key)
		if _, err := h.db.Exec(`INSERT INTO records VALUES ('a', ?, ?, 1)`, key, value); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	after := ""
	for page := 0; page < 100; page++ {
		out, err := h.db.storeScan(ctx, "a", map[string]any{"prefix": "thread/", "after": after})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(out)
		if len(raw) > scanPageBytes+1024 {
			t.Fatalf("page %d is %d bytes, over %d", page, len(raw), scanPageBytes+1024)
		}
		m := out.(map[string]any)
		records := m["records"].(map[string]any)
		if len(records) == 0 {
			t.Fatalf("page %d has no record", page)
		}
		var keys []string
		for k := range records {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		got = append(got, keys...)
		next, _ := m["next"].(string)
		if next == "" {
			break
		}
		if next != keys[len(keys)-1] {
			t.Fatalf("page %d: next = %q, want the last key %q", page, next, keys[len(keys)-1])
		}
		after = next
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scan returned %d keys, want %d in order", len(got), len(want))
	}
}

// One record that is larger than the page bound still comes back alone, so a scan never stops.
func TestStoreScanPageHasOneRecordAtLeast(t *testing.T) {
	h, _ := workerTestHub(t)
	ctx := context.Background()
	big := `"` + strings.Repeat("<", scanPageBytes/4) + `"` // about 1.5 x scanPageBytes encoded
	for _, key := range []string{"k/1", "k/2"} {
		if _, err := h.db.Exec(`INSERT INTO records VALUES ('a', ?, ?, 1)`, key, big); err != nil {
			t.Fatal(err)
		}
	}
	out, err := h.db.storeScan(ctx, "a", map[string]any{"prefix": "k/"})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if n := len(m["records"].(map[string]any)); n != 1 || m["next"] != "k/1" {
		t.Fatalf("records = %d, next = %v; want 1 record and next k/1", n, m["next"])
	}
}

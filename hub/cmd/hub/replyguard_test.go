package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

func frameOf(t *testing.T, reply map[string]any) int {
	t.Helper()
	raw, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	return rex.RequestFrameBytes("client.reply", raw)
}

// RequestFrameBytes counts the frame that Call writes, with the longest ID.
func TestRequestFrameBytesCountsTheFrame(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{"call_id": "c1", "result": map[string]any{"text": "<é\u2028\x01"}})
	id := "h" + strconv.FormatInt(1<<62, 10) // 19 digits
	frame, _ := json.Marshal(map[string]any{"type": "request", "id": id, "method": "client.reply", "payload": json.RawMessage(payload)})
	if got := rex.RequestFrameBytes("client.reply", payload); got != len(frame) {
		t.Fatalf("counted %d, the frame is %d", got, len(frame))
	}
}

func TestReplyLimitWithoutWelcomeLimits(t *testing.T) {
	if got := replyLimit(&rex.Rex{}); got != 64<<10-1<<10 {
		t.Fatalf("limit %d", got)
	}
}

// A reply at the limit is sent as is; one byte more becomes reply_too_large. Each content class
// is counted by its encoded size: '<' is 6 bytes, é is 2, U+2028 is 6, a control character is 6.
func TestGuardReplyBoundary(t *testing.T) {
	const limit = 100_000
	for _, unit := range []string{"a", "<", "é", " ", "\x01", `"\`} {
		for _, failed := range []bool{false, true} {
			make := func(n int, tail string) map[string]any {
				text := strings.Repeat(unit, n) + tail
				if failed {
					return map[string]any{"call_id": "call-7", "error": &rex.RexError{Code: "failed", Message: text}}
				}
				return map[string]any{"call_id": "call-7", "result": map[string]any{"text": text}}
			}
			base, one := frameOf(t, make(0, "")), frameOf(t, make(1, ""))-frameOf(t, make(0, ""))
			n := (limit - base) / one
			at := make(n, strings.Repeat("a", limit-base-n*one))
			if got := frameOf(t, at); got != limit {
				t.Fatalf("%q: setup frame %d", unit, got)
			}
			if raw, ok := guardReply("fix.get", at, limit); !ok || len(raw) != limit-rex.RequestFrameBytes("client.reply", nil) {
				t.Fatalf("%q failed=%v: a reply at the limit was replaced", unit, failed)
			}
			over := make(n, strings.Repeat("a", limit-base-n*one+1))
			raw, ok := guardReply("fix.get", over, limit)
			if ok {
				t.Fatalf("%q failed=%v: a reply over the limit was sent", unit, failed)
			}
			var got struct {
				CallID string        `json:"call_id"`
				Result any           `json:"result"`
				Error  *rex.RexError `json:"error"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.CallID != "call-7" || got.Result != nil || got.Error == nil || got.Error.Code != "reply_too_large" {
				t.Fatalf("%q: %s", unit, raw)
			}
			if !strings.Contains(got.Error.Message, "("+strconv.Itoa(limit+1)+" bytes, limit 100000)") {
				t.Fatalf("no attempted size: %s", got.Error.Message)
			}
			want := "returned a result"
			if failed {
				want = "returned an error"
			}
			if !strings.Contains(got.Error.Message, want) || !strings.Contains(got.Error.Message, "too large to send") ||
				!strings.Contains(got.Error.Message, "does not undo changes") || !strings.Contains(got.Error.Message, "keep its operation key") {
				t.Fatalf("%q failed=%v: %s", unit, failed, got.Error.Message)
			}
		}
	}
}

// The replacement error itself is small, also for a long action name.
func TestGuardReplyReplacementIsBounded(t *testing.T) {
	action := strings.Repeat("é", 5000)
	raw, ok := guardReply(action, map[string]any{"call_id": "c", "result": strings.Repeat("x", 2000)}, 1000)
	if ok || len(raw) > 600 {
		t.Fatalf("%d bytes: %s", len(raw), raw)
	}
}

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lazy-rex/LazyRex/hub/rex"
)

// TestShimIdentityLab runs the built shim against a fake hub on an isolated Rex lab
// (MCP_LAB_SOCKET=/tmp/rexlab-*/rex.sock, with no lazyrex-hub client). This test is the fake hub.
// Checks: startup joins, then discovers, with no identity report; each tool call with
// _meta.threadId reports that thread (kind codex, source mcp, the REX_BLOCK hint) right before
// its own dispatch, also with concurrent calls; a call without a thread reports nothing (the
// inherited CODEX_THREAD_ID is never used); a refused report is logged and the call still runs.
func TestShimIdentityLab(t *testing.T) {
	socket := os.Getenv("MCP_LAB_SOCKET")
	if socket == "" {
		t.Skip("set MCP_LAB_SOCKET to an isolated Rex lab socket")
	}
	if !strings.HasPrefix(socket, "/tmp/rexlab-") || !strings.HasSuffix(socket, "/rex.sock") {
		t.Fatal("refuse a non-lab socket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := rex.Dial(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if hubConnected(t, ctx, r) {
		t.Fatal("use a lab without a lazyrex-hub client")
	}
	if err := r.Call(ctx, "client.set_label", map[string]any{"label": hubLabel}, nil); err != nil {
		t.Fatal(err)
	}
	selfID := labelClient(t, ctx, r, hubLabel)
	var actions []map[string]any
	for _, n := range []string{"hub.join", "hub.tools", "hub.identity.report", "probe.echo"} {
		actions = append(actions, map[string]any{"name": n, "kind": "client", "title": n, "args": map[string]any{"type": "object"}})
	}
	if err := r.Call(ctx, "client.keymap", map[string]any{"actions": actions}, nil); err != nil {
		t.Fatal(err)
	}

	type call struct {
		Action string
		Args   map[string]any
		PID    int
	}
	var mu sync.Mutex
	var calls []call
	childPID := 0
	go func() {
		for raw := range r.Events {
			var env struct {
				Payload struct {
					Type    string          `json:"type"`
					Payload json.RawMessage `json:"payload"`
				} `json:"payload"`
			}
			if json.Unmarshal(raw, &env) != nil || env.Payload.Type != "client_perform_requested" {
				continue
			}
			var ev struct {
				CallID    string         `json:"call_id"`
				Action    string         `json:"action"`
				Args      map[string]any `json:"args"`
				Requester struct {
					Principal struct {
						PID int `json:"pid"`
					} `json:"principal"`
				} `json:"requester"`
			}
			if json.Unmarshal(env.Payload.Payload, &ev) != nil {
				continue
			}
			mu.Lock()
			calls = append(calls, call{ev.Action, ev.Args, ev.Requester.Principal.PID})
			mu.Unlock()
			reply := map[string]any{"call_id": ev.CallID}
			switch {
			case ev.Action == "hub.identity.report" && ev.Args["conversation_id"] == "stale":
				reply["error"] = map[string]any{"code": "stale", "message": "stale: old conversation"}
			case ev.Action == "hub.tools":
				reply["result"] = map[string]any{"tools": []any{map[string]any{"name": "probe_echo", "action": "probe.echo", "client_id": selfID}}}
			case ev.Action == "probe.echo":
				reply["result"] = ev.Args
			default:
				reply["result"] = map[string]any{}
			}
			r.Call(context.Background(), "client.reply", reply, nil)
		}
	}()

	bin := filepath.Join(t.TempDir(), "mcp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	child := exec.Command(bin, "-server", "unix://"+socket, "-agent", "lab")
	child.Env = append(os.Environ(), "LAZYREX_AGENT=lab", "REX_BLOCK=block:hint", "CODEX_THREAD_ID=stale-env", "CODEX_SESSION_ID=stale-env")
	stdin, _ := child.StdinPipe()
	stdout, _ := child.StdoutPipe()
	var stderr strings.Builder
	var errMu sync.Mutex
	child.Stderr = writerFunc(func(p []byte) (int, error) { errMu.Lock(); defer errMu.Unlock(); return stderr.Write(p) })
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	childPID = child.Process.Pid
	mu.Unlock()
	defer func() { stdin.Close(); child.Process.Kill(); child.Wait() }()

	var pmu sync.Mutex
	pending := map[float64]chan map[string]any{}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			id, _ := m["id"].(float64)
			pmu.Lock()
			ch := pending[id]
			delete(pending, id)
			pmu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}()
	var nextID float64
	var wmu sync.Mutex
	request := func(method string, params any) map[string]any {
		wmu.Lock()
		nextID++
		id := nextID
		ch := make(chan map[string]any, 1)
		pmu.Lock()
		pending[id] = ch
		pmu.Unlock()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		stdin.Write(append(b, '\n'))
		wmu.Unlock()
		select {
		case m := <-ch:
			return m
		case <-time.After(15 * time.Second):
			errMu.Lock()
			defer errMu.Unlock()
			t.Fatalf("MCP timeout: %s\n%s", method, stderr.String())
			return nil
		}
	}

	if m := request("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "identity-test", "version": "1"}}); m["error"] != nil {
		t.Fatalf("initialize: %v", m["error"])
	}
	stdin.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	list := request("tools/list", map[string]any{})
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "probe_echo" {
		t.Fatalf("tools/list: %v", list)
	}
	mu.Lock()
	startup := append([]call(nil), calls...)
	mu.Unlock()
	if len(startup) != 2 || startup[0].Action != "hub.join" || startup[1].Action != "hub.tools" ||
		!reflect.DeepEqual(startup[0].Args, map[string]any{"agent": "lab"}) {
		t.Fatalf("startup calls %+v, want hub.join {agent: lab} then hub.tools, and no report", startup)
	}

	invoke := func(value string, meta map[string]any) map[string]any {
		params := map[string]any{"name": "probe_echo", "arguments": map[string]any{"value": value}}
		if meta != nil {
			params["_meta"] = meta
		}
		return request("tools/call", params)
	}
	cases := []struct {
		value string
		meta  map[string]any
	}{
		{"first", map[string]any{"threadId": "conversation-a", "sessionId": "wrong-root"}},
		{"second", map[string]any{"threadId": "conversation-b"}},
		{"claude", nil},
		{"rejected-report", map[string]any{"threadId": "stale"}},
		{"invalid-meta", map[string]any{"threadId": 42}},
	}
	replies := make([]map[string]any, len(cases))
	var wg sync.WaitGroup
	for i, c := range cases {
		wg.Add(1)
		go func() { defer wg.Done(); replies[i] = invoke(c.value, c.meta) }()
	}
	wg.Wait()
	for i, m := range replies {
		res, _ := m["result"].(map[string]any)
		if res == nil || res["isError"] == true {
			t.Fatalf("%s: %v", cases[i].value, m)
		}
		var echo struct{ Value string }
		json.Unmarshal([]byte(res["content"].([]any)[0].(map[string]any)["text"].(string)), &echo)
		if echo.Value != cases[i].value {
			t.Errorf("%s: echo %q", cases[i].value, echo.Value)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	var reported []string
	want := map[string]string{"conversation-a": "first", "conversation-b": "second", "stale": "rejected-report"}
	for i, c := range calls {
		if c.PID != childPID {
			t.Errorf("%s came from pid %d, want the shim %d", c.Action, c.PID, childPID)
		}
		if c.Action != "hub.identity.report" {
			continue
		}
		conv, _ := c.Args["conversation_id"].(string)
		reported = append(reported, conv)
		if c.Args["kind"] != "codex" || c.Args["source"] != "mcp" || c.Args["block"] != "block:hint" {
			t.Errorf("report %v", c.Args)
		}
		if i+1 >= len(calls) || calls[i+1].Action != "probe.echo" || calls[i+1].Args["value"] != want[conv] {
			t.Errorf("report %s: the next call is %+v, want its own dispatch (value %s)", conv, calls[i+1:], want[conv])
		}
	}
	sort.Strings(reported)
	if !reflect.DeepEqual(reported, []string{"conversation-a", "conversation-b", "stale"}) {
		t.Errorf("reports %v: want one per call with a string threadId, never the inherited CODEX_THREAD_ID", reported)
	}
	errMu.Lock()
	defer errMu.Unlock()
	if !strings.Contains(stderr.String(), "conversation identity report:") || !strings.Contains(stderr.String(), "stale") {
		t.Errorf("the refused report is not logged:\n%s", stderr.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func hubConnected(t *testing.T, ctx context.Context, r *rex.Rex) bool {
	return labelClient(t, ctx, r, hubLabel) != ""
}

func labelClient(t *testing.T, ctx context.Context, r *rex.Rex, label string) string {
	t.Helper()
	var list struct {
		Clients []struct {
			ClientID        string `json:"client_id"`
			ConnectionState string `json:"connection_state"`
			Info            struct {
				Label string `json:"label"`
			} `json:"info"`
		} `json:"clients"`
	}
	if err := r.Call(ctx, "client.list", map[string]any{}, &list); err != nil {
		t.Fatal(fmt.Errorf("client.list: %w", err))
	}
	id := ""
	for _, c := range list.Clients {
		if c.Info.Label == label && c.ConnectionState == "connected" {
			id = c.ClientID
		}
	}
	return id
}

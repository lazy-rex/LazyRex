package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// rendererRun is the real lazyrex-view binary, attached to the fixture hub, with its stdout on a
// pty (v1.2 item 3). The input is a second pty that stays open, so only the output can fail;
// neither pty is the controlling terminal, so no SIGHUP ends the process first.
type rendererRun struct {
	t      *testing.T
	f      *protocolFixture
	cmd    *exec.Cmd
	stderr bytes.Buffer
	mu     sync.Mutex
	out    bytes.Buffer // every byte that the test read from the process's stdout
	closed chan struct{}
	done   chan error
}

func history(ids ...int) Snapshot {
	return treeOf(Node{ID: "history", Type: "history", PageSize: 500, Messages: msgs(ids...)})
}

// startRenderer builds the binary and starts it against f. When the output holds closeAfter, the
// reader closes the output pty's master: the pane goes away. The reader does the close itself,
// because a Close from another goroutine during a blocked Read is deferred until that Read
// returns, and the next print would still reach the master. With closeAfter "", the master is
// closed before the start, so the process's first write fails.
func startRenderer(t *testing.T, f *protocolFixture, closeAfter string) *rendererRun {
	dir, err := os.MkdirTemp("/tmp", "lr-exit-")
	if err != nil {
		t.Fatal(err)
	}
	r := &rendererRun{t: t, f: f, closed: make(chan struct{}), done: make(chan error, 1)}
	t.Cleanup(func() {
		if r.cmd != nil && r.cmd.Process != nil && r.cmd.ProcessState == nil {
			r.cmd.Process.Kill()
			<-r.done
		}
		if !t.Failed() {
			os.RemoveAll(dir)
		} else {
			t.Logf("kept %s", dir)
		}
	})
	bin := filepath.Join(dir, "lazyrex-view")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	sock := filepath.Join(dir, "rex.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go server.Serve(l)
	t.Cleanup(func() { server.Close() })

	r.cmd = exec.Command(bin, "-view-id", "v1", "-server", "unix://"+sock, "messaging", "/thread/42")
	r.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "REX_SESSION=session:lab", "REX_BLOCK=block:lab", "HOME=" + dir}
	inMaster, inSlave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inMaster.Close(); inSlave.Close() })
	master, outSlave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	pty.Setsize(master, &pty.Winsize{Rows: 20, Cols: 80})
	if closeAfter == "" {
		master.Close()
		close(r.closed)
	}
	r.cmd.Stdin, r.cmd.Stdout, r.cmd.Stderr = inSlave, outSlave, &r.stderr
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	outSlave.Close() // the child holds its own copy
	go func() { r.done <- r.cmd.Wait() }()
	if closeAfter != "" {
		go func() {
			b := make([]byte, 4096)
			for {
				n, err := master.Read(b)
				r.mu.Lock()
				r.out.Write(b[:n])
				found := strings.Contains(r.out.String(), closeAfter)
				r.mu.Unlock()
				if found || err != nil {
					master.Close()
					close(r.closed)
					return
				}
			}
		}()
	}
	return r
}

func (r *rendererRun) output() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out.String()
}

// waitClosed waits until the reader closed the master.
func (r *rendererRun) waitClosed(what string) {
	r.t.Helper()
	select {
	case <-r.closed:
	case <-time.After(10 * time.Second):
		r.t.Fatalf("%s was not printed: %q (stderr %q)", what, r.output(), r.stderr.String())
	}
}

func (r *rendererRun) seen() int {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	return r.f.seen
}

// exited waits for the process and checks exit status 1 and an empty stderr.
func (r *rendererRun) exited() {
	r.t.Helper()
	var err error
	select {
	case err = <-r.done:
	case <-time.After(15 * time.Second):
		// A goroutine dump on stderr shows where the process waits.
		r.cmd.Process.Signal(syscall.SIGQUIT)
		select {
		case err = <-r.done:
		case <-time.After(3 * time.Second):
			r.cmd.Process.Kill()
			err = <-r.done
		}
		r.f.mu.Lock()
		r.t.Logf("fixture: attaches %d, subscriptions %d, gets %d, seen %d", r.f.attaches, r.f.subscriptions, r.f.gets, r.f.seen)
		r.f.mu.Unlock()
		r.t.Fatalf("the renderer did not exit after the output fault (%v); stderr:\n%s", err, r.stderr.String())
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		r.t.Fatalf("the renderer ended with %v, want exit status 1", err)
	}
	if r.stderr.Len() != 0 {
		r.t.Fatalf("the renderer wrote to stderr after the fault: %q", r.stderr.String())
	}
	r.t.Logf("exit: %v; stdout before the close: %d bytes", err, len(r.output()))
}

// TestRendererExitsOnOutputFault (v1.2 item 3): the renderer prints a thread into a terminal. The
// pane goes away after m1, and the hub sends m2. The print's write fails (EIO), and the process
// exits with status 1, writes nothing to stderr and sends no seen.
func TestRendererExitsOnOutputFault(t *testing.T) {
	f := &protocolFixture{tree: history(1)}
	r := startRenderer(t, f, "TEXT-1")
	r.waitClosed("TEXT-1")
	f.mu.Lock()
	f.tree = history(1, 2)
	f.mu.Unlock()
	seenBefore := r.seen()
	f.changed(t)
	r.exited()
	if n := r.seen(); n != seenBefore {
		t.Fatalf("hub.view.seen after the fault: %d, before %d", n, seenBefore)
	}
}

// TestRendererClearsThePaneAtStart (lead-tv-clear2, lead-cur): the process's first bytes are the
// clear (cursor home, ESC[2J, ESC[3J) and the cursor hide (ESC[?25l), before any Bubble Tea byte,
// and no byte shows the cursor again. A reattach and a later print do not
// clear again: the process writes the clear once. The first-print probe of the review
// (TestReviewFirstPrintClearsOldScrollback) moves to this scope: main writes the clear, and the
// unit tests' startPrint does not run main.
func TestRendererClearsThePaneAtStart(t *testing.T) {
	f := &protocolFixture{tree: history(1)}
	r := startRenderer(t, f, "TEXT-2")
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(r.output(), "TEXT-1"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("TEXT-1 was not printed: %q (stderr %q)", r.output(), r.stderr.String())
		}
	}
	if out := r.output(); !strings.HasPrefix(out, clearScrollback+"\x1b[?25l") {
		t.Fatalf("the first bytes are not the clear and the cursor hide: %q", out[:min(len(out), 80)])
	}
	// A reattach: the hub's client goes away and comes back; the renderer attaches again.
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"type": "event", "payload": map[string]any{"type": "session_event", "payload": map[string]any{"event": map[string]any{"type": "client_disconnected", "payload": map[string]string{"client_id": "hub"}}}}})
	if err := conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		attaches := f.attaches
		f.mu.Unlock()
		if attaches >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the renderer did not attach again: %d attaches", attaches)
		}
	}
	f.mu.Lock()
	f.tree = history(1, 2)
	f.mu.Unlock()
	f.changed(t)
	r.waitClosed("TEXT-2")
	out := r.output()
	if n := strings.Count(out, clearScrollback); n != 1 {
		t.Fatalf("the clear is in the output %d times, want once: %q", n, out)
	}
	if n := strings.Count(out, "\x1b[3J"); n != 1 {
		t.Fatalf("ESC[3J is in the output %d times, want once", n)
	}
	if strings.Contains(out, "\x1b[?25h") {
		t.Fatalf("the output shows the cursor: %q", out)
	}
	t.Logf("stdout: %d bytes, the clear once, at the start; TEXT-1 and TEXT-2 printed", len(out))
}

// TestRendererExitsWhenTheStartClearFails (lead-tv-clear2): the pane is gone before the start. The
// clear's write fails, and the process exits with status 1 before Bubble Tea and the client
// start: no connection to the hub, nothing on stderr.
func TestRendererExitsWhenTheStartClearFails(t *testing.T) {
	f := &protocolFixture{tree: history(1)}
	r := startRenderer(t, f, "")
	r.exited()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns != 0 || f.attaches != 0 {
		t.Fatalf("the client started after a failed start clear: %d connections, %d attaches", f.conns, f.attaches)
	}
}

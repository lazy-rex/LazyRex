package main

import (
	"bytes"
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

	"github.com/creack/pty"
)

// TestRendererExitsOnOutputFault (v1.2 item 3): the real lazyrex-view binary, attached to the
// fixture hub, prints a thread into a terminal (a pty). The test closes the terminal's master side
// and the hub sends a new message. The print's write fails (EIO), and the process exits with
// status 1 and writes nothing to stderr. The input is a second pty that stays open, so only the
// output fails; neither pty is the controlling terminal, so no SIGHUP ends the process first.
func TestRendererExitsOnOutputFault(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "lr-exit-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if !t.Failed() {
			os.RemoveAll(dir)
		} else {
			t.Logf("kept %s", dir)
		}
	}()
	bin := filepath.Join(dir, "lazyrex-view")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	history := func(ids ...int) Snapshot {
		return treeOf(Node{ID: "history", Type: "history", PageSize: 500, Messages: msgs(ids...)})
	}
	f := &protocolFixture{tree: history(1)}
	sock := filepath.Join(dir, "rex.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go server.Serve(l)
	defer server.Close()

	cmd := exec.Command(bin, "-view-id", "v1", "-server", "unix://"+sock, "messaging", "/thread/42")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "REX_SESSION=session:lab", "REX_BLOCK=block:lab", "HOME=" + dir}
	inMaster, inSlave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer inMaster.Close()
	defer inSlave.Close()
	stdout, outSlave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	pty.Setsize(stdout, &pty.Winsize{Rows: 20, Cols: 80})
	cmd.Stdin, cmd.Stdout = inSlave, outSlave
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	outSlave.Close() // the child holds its own copy
	var mu sync.Mutex
	var seen bytes.Buffer
	// The reader closes the master itself after m1: a Close from another goroutine during a blocked
	// Read is deferred until that Read returns, and the next print would still reach the master.
	printed := make(chan struct{})
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := stdout.Read(b)
			mu.Lock()
			seen.Write(b[:n])
			found := strings.Contains(seen.String(), "TEXT-1")
			mu.Unlock()
			if found {
				stdout.Close() // the pane goes away
				close(printed)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-printed:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		mu.Lock()
		t.Fatalf("m1 was not printed: %q (stderr %q)", seen.String(), stderr.String())
	}
	f.mu.Lock()
	f.tree = history(1, 2)
	seenBefore := f.seen
	f.mu.Unlock()
	f.changed(t)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(15 * time.Second):
		// A goroutine dump on stderr shows where the process waits.
		cmd.Process.Signal(syscall.SIGQUIT)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
		}
		f.mu.Lock()
		t.Logf("fixture: attaches %d, subscriptions %d, gets %d, seen %d", f.attaches, f.subscriptions, f.gets, f.seen)
		f.mu.Unlock()
		t.Fatalf("the renderer did not exit after the output fault; stderr:\n%s", stderr.String())
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("the renderer ended with %v, want exit status 1", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("the renderer wrote to stderr after the fault: %q", stderr.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen != seenBefore {
		t.Fatalf("hub.view.seen after the fault: %d, before %d", f.seen, seenBefore)
	}
	mu.Lock()
	t.Logf("exit: %v; stdout before the close: %d bytes", err, seen.Len())
	mu.Unlock()
}

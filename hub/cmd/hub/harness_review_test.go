package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestReviewerRenderFailureRemainsFinal(t *testing.T) {
	h := &Hub{test: newTestState()}
	h.test.renderReq["view"] = 1
	h.testRenderDone("view", 1, false, "first render failed", 0, nil)
	h.testRenderDone("view", 1, true, "", 2, json.RawMessage(`{"title":"later success"}`))
	got, err := h.testRenderResult(map[string]any{"view_id": "view", "generation": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if got.(map[string]any)["status"] != "failed" {
		t.Fatalf("later unsolicited render erased failure: %v", got)
	}
}

func TestReviewerRenderGenerationKeepsItsOwnResult(t *testing.T) {
	h := &Hub{test: newTestState()}
	h.test.renderReq["view"] = 2
	h.testRenderDone("view", 1, false, "generation one failed", 0, nil)
	h.testRenderDone("view", 2, true, "", 2, json.RawMessage(`{"title":"second"}`))
	got, err := h.testRenderResult(map[string]any{"view_id": "view", "generation": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if got.(map[string]any)["status"] != "failed" {
		t.Fatalf("generation two erased generation one: %v", got)
	}
}

type reviewerReadError struct{}

func (reviewerReadError) Read([]byte) (int, error) { return 0, errors.New("reader failed") }
func TestReviewerParserRejectsReaderFailureAfterDone(t *testing.T) {
	r := io.MultiReader(strings.NewReader("LRT plan 1\nLRT ok 1 test\nLRT done 1 0\n"), reviewerReadError{})
	result := &fileResult{}
	(&testOpts{}).parse(r, result, io.Discard, nil)
	if result.ok() {
		t.Fatal("parser accepted a failed reader")
	}
}

func TestReviewerLoadActionRequiresParityOutsideHistory(t *testing.T) {
	d := actionDesc{Node: "form", Name: "load"}
	if parityProblem(d, nil, nil, nil) == "" {
		t.Fatal("arbitrary domain action named load bypasses parity")
	}
}

func TestReviewerMalformedNavigationIsNotDiscarded(t *testing.T) {
	for _, input := range []string{`{"navigate":{"plugin":"messaging","tab":7}}`, `{"navigate":{"plugin":7,"tab":"home"}}`,
		`{"navigate":true}`, `{"navigate":""}`, `{"navigate":{}}`, `{"navigate":[]}`, `{"form":7}`} {
		eff, err := decodeEffects([]byte(input))
		if err == nil && !eff.any() {
			t.Errorf("malformed effect became a successful no-op: %s", input)
		}
	}
	for _, input := range []string{`{}`, `[]`, `{"navigate":null}`, `{"ok":true}`} {
		eff, err := decodeEffects([]byte(input))
		if err != nil || eff.any() {
			t.Errorf("%s: want no effect, got %+v, %v", input, eff, err)
		}
	}
}

func TestReviewerSnapshotRejectsReusedRootPID(t *testing.T) {
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	// Model PID reuse without sending any signal: the recorded root lifetime differs from the
	// current process. Its child must not become a cleanup target.
	l := &lab{srv: &kept{pid: os.Getpid()}, tracked: map[int]string{os.Getpid(): "old-lifetime"}}
	l.snapshot()
	if _, claimed := l.tracked[child.Process.Pid]; claimed {
		t.Fatal("snapshot claimed a child of a root with a different process lifetime")
	}
	// With the right lifetime, the same child is claimed.
	l.tracked = map[int]string{os.Getpid(): startTime(os.Getpid())}
	l.snapshot()
	if _, claimed := l.tracked[child.Process.Pid]; !claimed {
		t.Fatal("snapshot did not claim a child of the recorded root")
	}
}

func TestWaiterSendsNoGroupSignalAfterReap(t *testing.T) {
	c := exec.Command("/usr/bin/true")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	w := watch(c)
	if err := <-w.done; err != nil {
		t.Fatal(err)
	}
	if w.signal(syscall.SIGKILL) {
		t.Fatal("signal sent to the group of a reaped leader")
	}
}

// The runner exits at once, and its child keeps the output open for 8 s. The wait must end at the
// timeout, not when the child exits.
func TestRunnerTimeoutEndsWhenAChildHoldsTheOutput(t *testing.T) {
	k, stdout := keptShell(t, "sleep 8 & exit 0")
	o := &testOpts{timeout: time.Second}
	l := &lab{tracked: map[int]string{}}
	res := &fileResult{}
	fctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	began := time.Now()
	o.waitRunner(context.Background(), fctx, l, k, stdout, res, io.Discard)
	if d := time.Since(began); d > 3*time.Second {
		t.Fatalf("the wait took %s", d)
	}
	if !strings.Contains(strings.Join(res.problems, "\n"), "timeout: the file did not finish") {
		t.Fatalf("problems: %v", res.problems)
	}
}

// A timeout or an interrupt during a hub restart: the restart gets a context that ends, and
// waitRunner returns only after the restart returns (the lab cleanup runs after it).
func TestRunnerEndDuringRestartWaitsForTheRestart(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		k, stdout := keptShell(t, "echo 'LRT plan 1'; echo 'LRT restart term'; exec sleep 30")
		var done atomic.Bool
		started := make(chan struct{})
		o := &testOpts{timeout: 500 * time.Millisecond}
		l := &lab{tracked: map[int]string{}}
		o.restartFn = func(ctx context.Context, l *lab, mode string) error {
			close(started)
			<-ctx.Done()
			l.restarts++ // the restart still uses the lab after the end of the file
			time.Sleep(500 * time.Millisecond)
			done.Store(true)
			return ctx.Err()
		}
		res := &fileResult{}
		ctx, stop := context.WithCancel(context.Background())
		fctx, cancel := context.WithTimeout(ctx, o.timeout)
		if interrupt {
			go func() { <-started; stop() }()
		}
		o.waitRunner(ctx, fctx, l, k, stdout, res, io.Discard)
		cancel()
		stop()
		if !done.Load() {
			t.Fatalf("interrupt=%v: waitRunner returned while the restart ran", interrupt)
		}
		want := "timeout: the file did not finish"
		if interrupt {
			want = "interrupted"
		}
		if !strings.Contains(strings.Join(res.problems, "\n"), want) {
			t.Fatalf("interrupt=%v: problems: %v", interrupt, res.problems)
		}
	}
}

// The reviewer's reproducer: a restart that still writes l.hub after the timeout. The timeout
// snapshot reads l.hub, so it must wait for the restart (go test -race).
func TestRunnerTimeoutSnapshotWaitsForTheRestart(t *testing.T) {
	k, stdout := keptShell(t, "echo 'LRT plan 1'; echo 'LRT restart term'; exec sleep 30")
	l := &lab{tracked: map[int]string{}}
	o := &testOpts{timeout: 200 * time.Millisecond}
	o.restartFn = func(ctx context.Context, l *lab, mode string) error {
		<-ctx.Done()
		until := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(until) {
			l.hub = k // as startHub assigns it
			runtime.Gosched()
		}
		return ctx.Err()
	}
	fctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	o.waitRunner(context.Background(), fctx, l, k, stdout, &fileResult{}, io.Discard)
}

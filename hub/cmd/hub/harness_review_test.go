package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
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
	(&testOpts{}).parse(r, result, io.Discard)
	if result.ok() {
		t.Fatal("parser accepted a failed reader")
	}
}

func TestReviewerLoadActionRequiresParityOutsideHistory(t *testing.T) {
	d := actionDesc{Node: "form", Name: "load"}
	if parityProblem(d, nil, nil) == "" {
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
	l := &lab{srv: &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}, tracked: map[int]string{os.Getpid(): "old-lifetime"}}
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

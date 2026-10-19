package main

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
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

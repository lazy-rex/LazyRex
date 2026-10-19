package main

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// composerFixtureSnapshot is a thread view with only its composer: the transcript pane
// shows the history.
func composerFixtureSnapshot(v *viewClient, rev int) Snapshot {
	return Snapshot{SchemaVersion: 1, ViewID: v.ViewID, Route: v.Route, Revision: "1", Tree: Tree{Title: "Demo thread", Nodes: []Node{
		{ID: "composer", Type: "composer", Label: "Message", DraftRevision: strconv.Itoa(rev),
			SaveAction: &Action{Name: "save", Label: "Save", Enabled: true}, SendAction: &Action{Name: "send", Label: "Send", Enabled: true}},
	}}}
}

// runFixtureComposer stands in for the hub in the demo. Draft saves succeed in memory.
// A send appends the text to the demo thread file as the user's message, where the
// transcript pane prints it.
func runFixtureComposer(ctx context.Context, v *viewClient, path string) {
	rev := 1
	v.emit(connectionMsg{Ready: true, Text: "Demo: no hub"})
	v.emit(composerFixtureSnapshot(v, rev))
	for {
		select {
		case <-ctx.Done():
			return
		case <-v.seen:
		case <-v.changed:
		case req := <-v.Requests:
			text, _ := req.Args["text"].(string)
			outcome := operationResult{Status: "succeeded", OperationKey: req.Key}
			switch req.Method {
			case "hub.view.draft.save":
				rev++
				outcome.Result = map[string]any{"draft_revision": strconv.Itoa(rev), "text": text}
			case "hub.view.send":
				now := time.Now().UTC()
				err := appendFixtureMessage(path, Message{ID: fmt.Sprintf("you-%d", now.UnixNano()), Author: "You", Own: true, Text: text, At: now.Format(time.RFC3339Nano)})
				if err != nil {
					outcome = operationResult{Status: "failed", OperationKey: req.Key, Error: &wireError{Code: "unavailable", Message: err.Error()}}
					break
				}
				rev++
				outcome.Result = map[string]any{"draft_revision": strconv.Itoa(rev), "text": ""}
			default:
				outcome = operationResult{Status: "failed", OperationKey: req.Key, Error: &wireError{Code: "not_supported", Message: "The demo supports only drafts and sends"}}
			}
			v.emit(completed{req, outcome})
		}
	}
}
